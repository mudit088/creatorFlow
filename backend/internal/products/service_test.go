package products

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/storage"
)

// These cover the two rules in DeleteFile that span tables and therefore cannot
// be database constraints: a sold file cannot be deleted, and removing the last
// deliverable un-publishes the product. Both are the kind of rule a later
// refactor quietly drops, which is the argument for pinning them here.

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

// fakeStore records deletions instead of talking to S3. What matters here is
// whether we asked it to delete at all — a refused deletion must not reach
// storage, or the row would survive while the bytes did not.
type fakeStore struct {
	deleted []string
}

func (f *fakeStore) PresignUpload(context.Context, string, string, int64, time.Duration) (string, error) {
	return "https://s3.test/upload", nil
}
func (f *fakeStore) StatObject(context.Context, string) (*storage.ObjectInfo, bool, error) {
	return &storage.ObjectInfo{Size: 1024}, true, nil
}
func (f *fakeStore) DeleteObject(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}

type fixture struct {
	userID    uuid.UUID
	profileID uuid.UUID
	productID uuid.UUID
	fileID    uuid.UUID
	s3Key     string
}

// newFixture builds a published product with one confirmed file.
func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	f := fixture{s3Key: "test/" + suffix + ".pdf"}

	must := func(step string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("user", pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("prod-test-%s@test.local", suffix)).Scan(&f.userID))
	must("profile", pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'Test') RETURNING id`,
		f.userID, "prodtest"+suffix).Scan(&f.profileID))
	must("product", pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor, status, published_at)
		 VALUES ($1, $2, 'Test Product', 49900, 'published', now()) RETURNING id`,
		f.profileID, "p-"+suffix).Scan(&f.productID))
	must("file", pool.QueryRow(ctx,
		`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes, uploaded_at)
		 VALUES ($1, $2, 'plan.pdf', 'application/pdf', 1024, now()) RETURNING id`,
		f.productID, f.s3Key).Scan(&f.fileID))

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM entitlements WHERE product_id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM orders WHERE profile_id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM products WHERE id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

// sell creates a paid order with an entitlement for the fixture's product.
func sell(t *testing.T, pool *pgxpool.Pool, f fixture, revoked bool) {
	t.Helper()
	ctx := context.Background()
	var orderID uuid.UUID

	if err := pool.QueryRow(ctx,
		`INSERT INTO orders (profile_id, buyer_email, total_minor, status, paid_at)
		 VALUES ($1, 'buyer@test.local', 49900, 'paid', now()) RETURNING id`,
		f.profileID).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, title_snapshot, unit_price_minor)
		 VALUES ($1, $2, 'Test Product', 49900)`, orderID, f.productID); err != nil {
		t.Fatalf("insert order item: %v", err)
	}

	revokedAt := "NULL"
	if revoked {
		revokedAt = "now()"
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO entitlements (order_id, product_id, buyer_email, revoked_at)
		 VALUES ($1, $2, 'buyer@test.local', %s)`, revokedAt), orderID, f.productID); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
}

func productStatus(t *testing.T, pool *pgxpool.Pool, productID uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM products WHERE id = $1`, productID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func fileCount(t *testing.T, pool *pgxpool.Pool, productID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM product_files WHERE product_id = $1`, productID).Scan(&n); err != nil {
		t.Fatalf("count files: %v", err)
	}
	return n
}

// The rule that protects a buyer from the seller.
func TestDeleteFileRefusedWhenSold(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	sell(t, pool, f, false)

	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store, nil)

	err := svc.DeleteFile(context.Background(), f.userID, f.productID, f.fileID)
	if !errors.Is(err, ErrFileSold) {
		t.Fatalf("error = %v, want ErrFileSold", err)
	}
	if n := fileCount(t, pool, f.productID); n != 1 {
		t.Errorf("files = %d, want the row kept", n)
	}
	if len(store.deleted) != 0 {
		t.Errorf("deleted %v from storage; a refused deletion must not reach S3", store.deleted)
	}
	if s := productStatus(t, pool, f.productID); s != StatusPublished {
		t.Errorf("status = %s, want it untouched", s)
	}
}

// A refunded buyer has no claim, so the creator can tidy up again.
func TestDeleteFileAllowedWhenEntitlementRevoked(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	sell(t, pool, f, true)

	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store, nil)

	if err := svc.DeleteFile(context.Background(), f.userID, f.productID, f.fileID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if n := fileCount(t, pool, f.productID); n != 0 {
		t.Errorf("files = %d, want 0", n)
	}
	if len(store.deleted) != 1 || store.deleted[0] != f.s3Key {
		t.Errorf("deleted = %v, want [%s]", store.deleted, f.s3Key)
	}
}

// The bug found in phase 9: a published product whose last file is removed used
// to stay published, so the storefront kept offering something checkout would
// then refuse to sell.
func TestDeletingLastFileUnpublishesProduct(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := NewService(NewRepository(pool), &fakeStore{}, nil)

	if err := svc.DeleteFile(context.Background(), f.userID, f.productID, f.fileID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if s := productStatus(t, pool, f.productID); s != StatusDraft {
		t.Errorf("status = %s, want draft once nothing is deliverable", s)
	}

	// The paired CHECK requires published_at to be NULL whenever status is not
	// published, so a half-applied change would have been rejected outright.
	var publishedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT published_at FROM products WHERE id = $1`, f.productID).Scan(&publishedAt); err != nil {
		t.Fatalf("read published_at: %v", err)
	}
	if publishedAt != nil {
		t.Errorf("published_at = %v, want NULL", publishedAt)
	}
}

// Deleting one of several files leaves the product sellable.
func TestDeletingOneOfTwoFilesKeepsProductPublished(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes, uploaded_at)
		 VALUES ($1, $2, 'bonus.pdf', 'application/pdf', 2048, now())`,
		f.productID, f.s3Key+"-bonus"); err != nil {
		t.Fatalf("insert second file: %v", err)
	}

	svc := NewService(NewRepository(pool), &fakeStore{}, nil)
	if err := svc.DeleteFile(context.Background(), f.userID, f.productID, f.fileID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if s := productStatus(t, pool, f.productID); s != StatusPublished {
		t.Errorf("status = %s, want published — a deliverable remains", s)
	}
}

// Someone else's file is not deletable, and is reported as missing rather than
// forbidden: confirming the id exists is information we do not owe anyone.
func TestDeleteFileScopedToOwner(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	other := newFixture(t, pool)

	store := &fakeStore{}
	svc := NewService(NewRepository(pool), store, nil)

	err := svc.DeleteFile(context.Background(), other.userID, f.productID, f.fileID)
	if !errors.Is(err, ErrProductNotFound) && !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("error = %v, want a not-found error", err)
	}
	if n := fileCount(t, pool, f.productID); n != 1 {
		t.Errorf("files = %d, want the owner's file untouched", n)
	}
	if len(store.deleted) != 0 {
		t.Errorf("deleted %v from storage for a non-owner", store.deleted)
	}
}
