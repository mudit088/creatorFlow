package products

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

func (r *Repository) InTx(ctx context.Context, fn func(*Repository) error) error {
	return database.RunInTx(ctx, r.pool, func(q database.Querier) error {
		return fn(&Repository{db: q, pool: r.pool})
	})
}

const productColumns = `id, profile_id, slug, title, description, price_minor, currency, status, published_at, created_at, updated_at`

func scanProduct(row pgx.Row) (*Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.ProfileID, &p.Slug, &p.Title, &p.Description,
		&p.PriceMinor, &p.Currency, &p.Status, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

// Create resolves the caller's profile inside the INSERT itself.
//
// INSERT ... SELECT means there is no separate "which profile does this user
// own?" round trip, and no window between that lookup and the write. A user
// with no profile matches no row, so nothing is inserted and pgx reports no
// rows, which is the only way this statement can affect zero rows.
//
// This is also why the package never imports profiles: ownership is expressed
// in SQL, so products needs no knowledge of that package's types or errors.
func (r *Repository) Create(ctx context.Context, userID uuid.UUID, slug, title string, description *string, priceMinor int64) (*Product, error) {
	const q = `
		INSERT INTO products (profile_id, slug, title, description, price_minor)
		SELECT id, $2, $3, $4, $5 FROM profiles WHERE user_id = $1
		RETURNING ` + productColumns

	p, err := scanProduct(r.db.QueryRow(ctx, q, userID, slug, title, description, priceMinor))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileRequired
	}
	if err != nil {
		return nil, translateProductError(err)
	}
	return p, nil
}

// FindForUser scopes by the caller's profile in the same statement that fetches
// the row. A product belonging to someone else matches nothing and is reported
// as not found, which is also the right answer to give: confirming that an id
// exists but belongs to another creator is information we do not owe anyone.
func (r *Repository) FindForUser(ctx context.Context, userID, productID uuid.UUID) (*Product, error) {
	const q = `
		SELECT ` + productColumns + ` FROM products
		WHERE id = $2 AND profile_id = (SELECT id FROM profiles WHERE user_id = $1)`

	p, err := scanProduct(r.db.QueryRow(ctx, q, userID, productID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProductNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find product: %w", err)
	}
	return p, nil
}

func (r *Repository) ListForUser(ctx context.Context, userID uuid.UUID) ([]Product, error) {
	const q = `
		SELECT ` + productColumns + ` FROM products
		WHERE profile_id = (SELECT id FROM profiles WHERE user_id = $1)
		ORDER BY created_at DESC`

	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()

	out := make([]Product, 0)
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Update applies a partial change. published_at is derived from status rather
// than accepted from the client, because products_published_consistent requires
// the two to agree and a client has no business choosing a publication time.
func (r *Repository) Update(ctx context.Context, userID, productID uuid.UUID, title, description, slug *string, priceMinor *int64, status *string) (*Product, error) {
	const q = `
		UPDATE products SET
			title        = COALESCE($3, title),
			description  = COALESCE($4, description),
			slug         = COALESCE($5, slug),
			price_minor  = COALESCE($6, price_minor),
			status       = COALESCE($7, status),
			published_at = CASE
				WHEN COALESCE($7, status) = 'published' THEN COALESCE(published_at, now())
				ELSE NULL
			END
		WHERE id = $2 AND profile_id = (SELECT id FROM profiles WHERE user_id = $1)
		RETURNING ` + productColumns

	p, err := scanProduct(r.db.QueryRow(ctx, q, userID, productID, title, description, slug, priceMinor, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProductNotFound
	}
	if err != nil {
		return nil, translateProductError(err)
	}
	return p, nil
}

func translateProductError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("product write: %w", err)
	}

	switch pgErr.ConstraintName {
	case "products_slug_unique":
		return ErrSlugTaken
	case "products_slug_shape":
		return &ValidationError{"slug", "Slug must be 2 to 60 characters of lowercase letters, numbers and hyphens."}
	case "products_title_len":
		return &ValidationError{"title", "Title must be between 1 and 120 characters."}
	case "products_price_nonneg", "products_price_max":
		return &ValidationError{"price_minor", "Price is out of range."}
	case "products_status_valid":
		return &ValidationError{"status", "Status must be draft, published or archived."}
	default:
		return fmt.Errorf("product write: %w", err)
	}
}

// PublicCoords returns the username and slug a product is published under — the
// two values that identify its pages in the storefront cache. One indexed join
// on a write path, which is where this system can afford a query.
func (r *Repository) PublicCoords(ctx context.Context, productID uuid.UUID) (username, slug string, err error) {
	const q = `
		SELECT pr.username, p.slug
		FROM products p
		JOIN profiles pr ON pr.id = p.profile_id
		WHERE p.id = $1`

	if err := r.db.QueryRow(ctx, q, productID).Scan(&username, &slug); err != nil {
		return "", "", fmt.Errorf("public coords: %w", err)
	}
	return username, slug, nil
}
