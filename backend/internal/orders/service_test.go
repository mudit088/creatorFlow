package orders

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/razorpay"
)

// These run against a real PostgreSQL, because what is being tested is
// behaviour the database owns: a unique index deciding a race, a transaction
// that must commit before a third party is called, and a pending row surviving
// a provider failure. A mocked database would test the mock.
//
// Skipped when DATABASE_URL is absent, so `go test ./...` still passes on a
// machine with no infrastructure running.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping database-backed test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("database unreachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixture is one creator with one published, deliverable product — the minimum
// state in which a purchase is legal.
type fixture struct {
	userID    uuid.UUID
	profileID uuid.UUID
	productID uuid.UUID
	price     int64
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	f := fixture{price: 49900}

	err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("orders-test-%s@test.local", suffix)).Scan(&f.userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	err = pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'Test') RETURNING id`,
		f.userID, "ordtest"+suffix).Scan(&f.profileID)
	if err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	err = pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor, status, published_at)
		 VALUES ($1, $2, 'Test Product', $3, 'published', now()) RETURNING id`,
		f.profileID, "p-"+suffix, f.price).Scan(&f.productID)
	if err != nil {
		t.Fatalf("insert product: %v", err)
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes, uploaded_at)
		 VALUES ($1, $2, 'f.pdf', 'application/pdf', 1024, now())`,
		f.productID, "test/"+suffix+".pdf")
	if err != nil {
		t.Fatalf("insert product file: %v", err)
	}

	// Ordered by the foreign keys: orders hold RESTRICT references to products
	// and profiles, so they have to go first or the cleanup fails the same way a
	// real account deletion would.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM orders WHERE profile_id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM products WHERE profile_id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, f.userID)
	})

	return f
}

// fakeProvider stands in for Razorpay. This is the entire reason PaymentProvider
// is an interface declared in this package: the checkout logic can be driven
// through its failure paths without an account, a network or a test card.
type fakeProvider struct {
	enabled bool
	fail    error
	calls   int
	receipt string
	amount  int64
}

func (f *fakeProvider) Enabled() bool { return f.enabled }
func (f *fakeProvider) KeyID() string { return "rzp_test_fake" }
func (f *fakeProvider) CreateOrder(_ context.Context, amountMinor int64, currency, receipt string, _ map[string]string) (*razorpay.Order, error) {
	f.calls++
	f.receipt = receipt
	f.amount = amountMinor
	if f.fail != nil {
		return nil, f.fail
	}
	return &razorpay.Order{
		ID:       fmt.Sprintf("order_fake_%d", f.calls),
		Amount:   amountMinor,
		Currency: currency,
		Receipt:  receipt,
		Status:   "created",
	}, nil
}

func TestCreateAttachesProviderOrder(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)

	provider := &fakeProvider{enabled: true}
	svc := NewService(NewRepository(pool), provider)

	order, items, replayed, err := svc.Create(context.Background(), "buyer@test.local", []string{f.productID.String()}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if replayed {
		t.Error("replayed = true on a first request")
	}
	if order.ProviderOrderID == nil || *order.ProviderOrderID != "order_fake_1" {
		t.Errorf("provider order id = %v, want order_fake_1", order.ProviderOrderID)
	}
	// The amount sent to the provider must be the amount the database computed,
	// not anything the caller supplied.
	if provider.amount != f.price {
		t.Errorf("amount sent to provider = %d, want %d", provider.amount, f.price)
	}
	// The receipt ties the provider's record back to ours.
	if provider.receipt != order.ID.String() {
		t.Errorf("receipt = %q, want our order id %q", provider.receipt, order.ID)
	}
	if len(items) != 1 || items[0].UnitPriceMinor != f.price {
		t.Errorf("items = %+v, want one line at %d", items, f.price)
	}
}

// The failure that actually matters: Razorpay is down after our order committed.
// The order must survive as pending, and retrying the same request must finish
// it rather than create a second one.
func TestProviderFailureLeavesPendingOrderThatRetryCompletes(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	provider := &fakeProvider{enabled: true, fail: errors.New("connection refused")}
	svc := NewService(NewRepository(pool), provider)

	key := "retry-" + uuid.NewString()
	_, _, _, err := svc.Create(ctx, "buyer@test.local", []string{f.productID.String()}, &key)
	if !errors.Is(err, ErrPaymentProviderUnavailable) {
		t.Fatalf("error = %v, want ErrPaymentProviderUnavailable", err)
	}

	// The local order exists, pending, with no provider id — recoverable rather
	// than lost.
	var count int
	var status string
	var providerID *string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(status), max(provider_order_id) FROM orders WHERE idempotency_key = $1`,
		key).Scan(&count, &status, &providerID); err != nil {
		t.Fatalf("query order: %v", err)
	}
	if count != 1 || status != StatusPending || providerID != nil {
		t.Fatalf("after failure: count=%d status=%q provider=%v, want 1/pending/nil", count, status, providerID)
	}

	// Razorpay comes back; the client retries the identical request.
	provider.fail = nil
	order, _, replayed, err := svc.Create(ctx, "buyer@test.local", []string{f.productID.String()}, &key)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !replayed {
		t.Error("replayed = false; the retry created a new order instead of resuming")
	}
	if order.ProviderOrderID == nil {
		t.Error("retry did not attach a provider order")
	}

	// Still exactly one order for this key.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Errorf("orders for key = %d, want 1", count)
	}
}

// With no credentials the checkout still has to work, or nobody can develop
// against it. The order is real; only the payment is unavailable.
func TestCreateWithoutConfiguredProvider(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)

	provider := &fakeProvider{enabled: false}
	svc := NewService(NewRepository(pool), provider)

	order, _, _, err := svc.Create(context.Background(), "buyer@test.local", []string{f.productID.String()}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if order.ProviderOrderID != nil {
		t.Errorf("provider order id = %v, want nil when unconfigured", order.ProviderOrderID)
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d times while disabled", provider.calls)
	}
	if svc.PublicKeyID() != "" {
		t.Errorf("PublicKeyID = %q, want empty while disabled", svc.PublicKeyID())
	}
}

// An order already carrying a provider id must not get a second one. This is
// what makes the replay path safe to run on every retry.
func TestEnsureProviderOrderIsIdempotent(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	provider := &fakeProvider{enabled: true}
	svc := NewService(NewRepository(pool), provider)

	key := "idem-" + uuid.NewString()
	first, _, _, err := svc.Create(ctx, "buyer@test.local", []string{f.productID.String()}, &key)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	second, _, replayed, err := svc.Create(ctx, "buyer@test.local", []string{f.productID.String()}, &key)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed || second.ID != first.ID {
		t.Errorf("replay returned %v (replayed=%v), want the original %v", second.ID, replayed, first.ID)
	}
	if provider.calls != 1 {
		t.Errorf("provider called %d times, want 1 — a replay must not create a second provider order", provider.calls)
	}
}
