package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/database"
)

type Repository struct {
	db   database.Querier
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool, pool: pool}
}

// InTx rebinds the repository to a transaction. Checkout needs it: the order
// row and its items are one fact, and an order whose items failed to insert is
// an order for nothing that a buyer can still be charged for.
func (r *Repository) InTx(ctx context.Context, fn func(*Repository) error) error {
	return database.RunInTx(ctx, r.pool, func(q database.Querier) error {
		return fn(&Repository{db: q, pool: r.pool})
	})
}

const orderColumns = `id, profile_id, buyer_email, status, total_minor, currency,
	provider_order_id, idempotency_key, paid_at, created_at, updated_at`

func scanOrder(row pgx.Row) (*Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.ProfileID, &o.BuyerEmail, &o.Status, &o.TotalMinor, &o.Currency,
		&o.ProviderOrderID, &o.IdempotencyKey, &o.PaidAt, &o.CreatedAt, &o.UpdatedAt)
	return &o, err
}

// LoadPurchasable is the only place a price is ever obtained. The request is not
// asked what anything costs, because the request comes from a browser and a
// browser is an adversary: "amount" in a request body is the single most
// reliably exploited field in any checkout.
//
// The EXISTS clause is not belt-and-braces. products.DeleteFile removes a file
// without re-checking the product's status, so a published product can be left
// with nothing behind it. Selling that would take a buyer's money for something
// phase 9 could never hand over, so this query refuses to price it.
func (r *Repository) LoadPurchasable(ctx context.Context, productIDs []string) ([]purchasable, error) {
	const q = `
		SELECT p.id, p.profile_id, p.title, p.price_minor, p.currency
		FROM products p
		WHERE p.id = ANY($1::uuid[])
		  AND p.status = 'published'
		  AND EXISTS (
		      SELECT 1 FROM product_files f
		      WHERE f.product_id = p.id AND f.uploaded_at IS NOT NULL
		  )`

	rows, err := r.db.Query(ctx, q, productIDs)
	if err != nil {
		return nil, fmt.Errorf("load purchasable products: %w", err)
	}
	defer rows.Close()

	var out []purchasable
	for rows.Next() {
		var p purchasable
		if err := rows.Scan(&p.ID, &p.ProfileID, &p.Title, &p.PriceMinor, &p.Currency); err != nil {
			return nil, fmt.Errorf("scan purchasable product: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// errIdempotencyConflict is package-internal: it means "this key already made an
// order", which the service turns into a replay rather than an error the caller
// ever sees.
var (
	errIdempotencyConflict = errors.New("idempotency key already used")
	// errProviderOrderAlreadySet means another request won the race to attach a
	// provider order. Not a failure: the caller re-reads the row and uses theirs.
	errProviderOrderAlreadySet = errors.New("provider order already attached")
)

func (r *Repository) CreateOrder(ctx context.Context, profileID uuid.UUID, buyerEmail string, totalMinor int64, currency string, idempotencyKey *string) (*Order, error) {
	const q = `
		INSERT INTO orders (profile_id, buyer_email, total_minor, currency, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + orderColumns

	o, err := scanOrder(r.db.QueryRow(ctx, q, profileID, buyerEmail, totalMinor, currency, idempotencyKey))
	if err != nil {
		return nil, translateOrderError(err)
	}
	return o, nil
}

// AddItem inserts one frozen line. Called in a loop inside the checkout
// transaction: a cart is at most maxItemsPerOrder rows, so the round trips are
// bounded and cost less than the readability of a hand-built multi-row INSERT.
func (r *Repository) AddItem(ctx context.Context, orderID, productID uuid.UUID, title string, unitPriceMinor int64) error {
	const q = `
		INSERT INTO order_items (order_id, product_id, title_snapshot, unit_price_minor)
		VALUES ($1, $2, $3, $4)`

	if _, err := r.db.Exec(ctx, q, orderID, productID, title, unitPriceMinor); err != nil {
		return fmt.Errorf("insert order item: %w", err)
	}
	return nil
}

func (r *Repository) FindByIdempotencyKey(ctx context.Context, key string) (*Order, error) {
	const q = `SELECT ` + orderColumns + ` FROM orders WHERE idempotency_key = $1`

	o, err := scanOrder(r.db.QueryRow(ctx, q, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find order by idempotency key: %w", err)
	}
	return o, nil
}

func (r *Repository) ListItems(ctx context.Context, orderID uuid.UUID) ([]Item, error) {
	const q = `
		SELECT id, order_id, product_id, title_snapshot, unit_price_minor, quantity
		FROM order_items WHERE order_id = $1 ORDER BY created_at`

	rows, err := r.db.Query(ctx, q, orderID)
	if err != nil {
		return nil, fmt.Errorf("list order items: %w", err)
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var i Item
		if err := rows.Scan(&i.ID, &i.OrderID, &i.ProductID, &i.TitleSnapshot, &i.UnitPriceMinor, &i.Quantity); err != nil {
			return nil, fmt.Errorf("scan order item: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// translateOrderError keeps pgx below this line. A caller above the repository
// works in domain errors and never learns that Postgres has error codes.
func translateOrderError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "orders_idempotency_key_unique" {
			return errIdempotencyConflict
		}
	}
	return fmt.Errorf("create order: %w", err)
}

// AttachProviderOrder records the Razorpay order id against our row.
//
// The WHERE clause carries "AND provider_order_id IS NULL" so this can never
// overwrite an id that is already there. Two concurrent retries can each create
// a Razorpay order; clobbering the stored id would abandon the one the buyer's
// checkout window is actually using, and the payment would arrive referencing an
// order we no longer point at. The loser of that race is told to go and read the
// winner's row instead.
func (r *Repository) AttachProviderOrder(ctx context.Context, orderID uuid.UUID, providerOrderID string) (*Order, error) {
	const q = `
		UPDATE orders SET provider_order_id = $2
		WHERE id = $1 AND provider_order_id IS NULL
		RETURNING ` + orderColumns

	o, err := scanOrder(r.db.QueryRow(ctx, q, orderID, providerOrderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errProviderOrderAlreadySet
	}
	if err != nil {
		return nil, fmt.Errorf("attach provider order: %w", err)
	}
	return o, nil
}

func (r *Repository) FindByID(ctx context.Context, orderID uuid.UUID) (*Order, error) {
	const q = `SELECT ` + orderColumns + ` FROM orders WHERE id = $1`

	o, err := scanOrder(r.db.QueryRow(ctx, q, orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find order: %w", err)
	}
	return o, nil
}
