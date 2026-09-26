package delivery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Delivery decides who may have a paid-for file, so these run against a real
// PostgreSQL: the authorization lives in a SQL WHERE clause, and a mocked
// database would only prove the mock agrees with itself.
//
// The object store is faked, because presigning is AWS's code, not ours — what
// matters here is which keys we ask it to sign, and that we ask at all only
// after the entitlement check passed.

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

type fakeStore struct {
	signed []string
	fail   error
}

func (f *fakeStore) PresignDownload(_ context.Context, key, downloadAs string, ttl time.Duration) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	f.signed = append(f.signed, key)
	return fmt.Sprintf("https://s3.test/%s?filename=%s&ttl=%d", key, downloadAs, int(ttl.Seconds())), nil
}

// fixture is a paid order with one entitlement and one confirmed file — a buyer
// who has paid and should be able to download.
type fixture struct {
	orderID    uuid.UUID
	productID  uuid.UUID
	profileID  uuid.UUID
	userID     uuid.UUID
	buyerEmail string
	s3Key      string
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	f := fixture{
		buyerEmail: fmt.Sprintf("buyer-%s@test.local", suffix),
		s3Key:      "test/" + suffix + ".pdf",
	}

	must := func(step string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("user", pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("del-test-%s@test.local", suffix)).Scan(&f.userID))
	must("profile", pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'Test') RETURNING id`,
		f.userID, "deltest"+suffix).Scan(&f.profileID))
	must("product", pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor, status, published_at)
		 VALUES ($1, $2, 'Test Product', 49900, 'published', now()) RETURNING id`,
		f.profileID, "p-"+suffix).Scan(&f.productID))

	_, err := pool.Exec(ctx,
		`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes, uploaded_at)
		 VALUES ($1, $2, 'plan.pdf', 'application/pdf', 1024, now())`, f.productID, f.s3Key)
	must("file", err)

	must("order", pool.QueryRow(ctx,
		`INSERT INTO orders (profile_id, buyer_email, total_minor, status, paid_at)
		 VALUES ($1, $2, 49900, 'paid', now()) RETURNING id`,
		f.profileID, f.buyerEmail).Scan(&f.orderID))

	_, err = pool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, title_snapshot, unit_price_minor)
		 VALUES ($1, $2, 'Test Product', 49900)`, f.orderID, f.productID)
	must("order item", err)

	_, err = pool.Exec(ctx,
		`INSERT INTO entitlements (order_id, product_id, buyer_email) VALUES ($1, $2, $3)`,
		f.orderID, f.productID, f.buyerEmail)
	must("entitlement", err)

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM entitlements WHERE order_id = $1`, f.orderID)
		_, _ = pool.Exec(c, `DELETE FROM orders WHERE id = $1`, f.orderID)
		_, _ = pool.Exec(c, `DELETE FROM products WHERE id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, f.userID)
	})

	return f
}

func TestEntitledBuyerGetsSignedLinks(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store)

	links, err := svc.Download(context.Background(), f.orderID, f.buyerEmail)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	if links[0].Filename != "plan.pdf" {
		t.Errorf("filename = %q, want the original name, not the storage key", links[0].Filename)
	}
	if links[0].ExpiresIn != downloadTTL {
		t.Errorf("expires_in = %v, want %v", links[0].ExpiresIn, downloadTTL)
	}
	if len(store.signed) != 1 || store.signed[0] != f.s3Key {
		t.Errorf("signed keys = %v, want [%s]", store.signed, f.s3Key)
	}
}

// citext: a case difference between checkout and the download form must not cost
// someone the thing they paid for.
func TestEmailMatchIsCaseInsensitive(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := NewService(NewRepository(pool), &fakeStore{})

	shouty := ""
	for _, r := range f.buyerEmail {
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		shouty += string(r)
	}

	links, err := svc.Download(context.Background(), f.orderID, shouty)
	if err != nil {
		t.Fatalf("Download with %q: %v", shouty, err)
	}
	if len(links) != 1 {
		t.Errorf("links = %d, want 1", len(links))
	}
}

// The second factor doing its job: holding the order id is not enough.
func TestWrongEmailIsRefused(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store)

	_, err := svc.Download(context.Background(), f.orderID, "attacker@evil.test")
	if !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("error = %v, want ErrNotEntitled", err)
	}
	if len(store.signed) != 0 {
		t.Errorf("signed %d keys for an unentitled caller; nothing should be signed", len(store.signed))
	}
}

// A refund revokes access. The row stays for the audit trail, so "revoked on the
// 4th" remains answerable, but the file stops being downloadable.
func TestRevokedEntitlementIsRefused(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := NewService(NewRepository(pool), &fakeStore{})

	if _, err := pool.Exec(context.Background(),
		`UPDATE entitlements SET revoked_at = now() WHERE order_id = $1`, f.orderID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := svc.Download(context.Background(), f.orderID, f.buyerEmail); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("error = %v, want ErrNotEntitled after revocation", err)
	}
}

// An order that was never paid has no entitlement row, so it is refused by the
// same rule rather than by a separate status check.
func TestUnpaidOrderHasNothingToDownload(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := NewService(NewRepository(pool), &fakeStore{})

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM entitlements WHERE order_id = $1`, f.orderID); err != nil {
		t.Fatalf("delete entitlement: %v", err)
	}

	if _, err := svc.Download(context.Background(), f.orderID, f.buyerEmail); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("error = %v, want ErrNotEntitled", err)
	}
}

// A file whose upload was never confirmed has no bytes behind it. Signing a URL
// for it would hand the buyer a 404 from S3 instead of an honest answer.
func TestUnconfirmedFileIsNotOffered(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store)

	if _, err := pool.Exec(ctx,
		`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes)
		 VALUES ($1, $2, 'pending.pdf', 'application/pdf', 2048)`,
		f.productID, f.s3Key+".pending"); err != nil {
		t.Fatalf("insert pending file: %v", err)
	}

	links, err := svc.Download(ctx, f.orderID, f.buyerEmail)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1 — the unconfirmed file must not be offered", len(links))
	}
	if links[0].Filename != "plan.pdf" {
		t.Errorf("offered %q, want the confirmed file", links[0].Filename)
	}
}

// A nonexistent order and a wrong email must be indistinguishable, or the
// endpoint becomes a way to confirm which orders exist.
func TestUnknownOrderIsRefusedIdentically(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := NewService(NewRepository(pool), &fakeStore{})

	_, unknownOrder := svc.Download(context.Background(), uuid.New(), f.buyerEmail)
	_, wrongEmail := svc.Download(context.Background(), f.orderID, "attacker@evil.test")

	if !errors.Is(unknownOrder, ErrNotEntitled) || !errors.Is(wrongEmail, ErrNotEntitled) {
		t.Fatalf("unknown order = %v, wrong email = %v; both must be ErrNotEntitled", unknownOrder, wrongEmail)
	}
	// Identical text, not merely the same sentinel: the caller must not be able
	// to tell the two cases apart from the response either.
	if unknownOrder.Error() != wrongEmail.Error() {
		t.Errorf("messages differ: %q vs %q", unknownOrder, wrongEmail)
	}
}

// A malformed address is refused with the same error as a genuine miss, so it
// cannot be used to probe whether an order exists.
func TestMalformedEmailIsRefusedWithoutQueryingStore(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store)

	if _, err := svc.Download(context.Background(), f.orderID, "not-an-email"); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("error = %v, want ErrNotEntitled", err)
	}
	if len(store.signed) != 0 {
		t.Errorf("signed %d keys for a malformed email", len(store.signed))
	}
}
