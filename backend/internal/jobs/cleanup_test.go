package jobs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

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

type fixture struct {
	userID    uuid.UUID
	profileID uuid.UUID
	productID uuid.UUID
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var f fixture

	must := func(step string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("user", pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("job-test-%s@test.local", suffix)).Scan(&f.userID))
	must("profile", pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'J') RETURNING id`,
		f.userID, "jobtest"+suffix).Scan(&f.profileID))
	must("product", pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor) VALUES ($1, $2, 'P', 49900) RETURNING id`,
		f.profileID, "p-"+suffix).Scan(&f.productID))

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM events WHERE profile_id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM visitors WHERE profile_id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM orders WHERE profile_id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM product_files WHERE product_id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM products WHERE id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

func (f fixture) order(t *testing.T, pool *pgxpool.Pool, status string, createdAt time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	paidAt := "NULL"
	if status == "paid" {
		paidAt = "now()"
	}
	q := fmt.Sprintf(
		`INSERT INTO orders (profile_id, buyer_email, total_minor, status, created_at, paid_at)
		 VALUES ($1, 'buyer@test.local', 49900, $2, $3, %s) RETURNING id`, paidAt)
	if err := pool.QueryRow(context.Background(), q, f.profileID, status, createdAt).Scan(&id); err != nil {
		t.Fatalf("insert %s order: %v", status, err)
	}
	return id
}

func orderStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read order: %v", err)
	}
	return status
}

func TestExpirePendingOrdersOnlyTouchesStaleUnpaidOnes(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	c := NewCleanup(pool)

	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	stale := f.order(t, pool, "pending", old)
	fresh := f.order(t, pool, "pending", recent)
	paid := f.order(t, pool, "paid", old)

	if _, err := c.ExpirePendingOrders(context.Background()); err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}

	if got := orderStatus(t, pool, stale); got != "cancelled" {
		t.Errorf("stale pending order = %s, want cancelled", got)
	}
	// A buyer still on the payment page must not have their order cancelled
	// underneath them.
	if got := orderStatus(t, pool, fresh); got != "pending" {
		t.Errorf("recent pending order = %s, want pending", got)
	}
	// The one that matters most: money already taken is never touched.
	if got := orderStatus(t, pool, paid); got != "paid" {
		t.Errorf("paid order = %s, want paid — a cleanup job must never unwind a payment", got)
	}
}

// Cancelled, not deleted: an abandoned checkout is evidence, and a creator
// asking "did anyone try to buy this?" deserves an answer.
func TestExpiredOrdersAreKeptNotDeleted(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	c := NewCleanup(pool)

	id := f.order(t, pool, "pending", time.Now().Add(-48*time.Hour))
	if _, err := c.ExpirePendingOrders(context.Background()); err != nil {
		t.Fatalf("ExpirePendingOrders: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM orders WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("check order: %v", err)
	}
	if !exists {
		t.Error("the order row was deleted; it should be cancelled and kept")
	}
}

func TestSweepAbandonedUploadsLeavesConfirmedFiles(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	var deleted []string
	c := NewCleanup(pool)
	c.DeleteObject = func(_ context.Context, key string) error {
		deleted = append(deleted, key)
		return nil
	}

	old := time.Now().Add(-48 * time.Hour)
	insert := func(key string, uploaded bool, createdAt time.Time) {
		uploadedAt := "NULL"
		if uploaded {
			uploadedAt = "now()"
		}
		q := fmt.Sprintf(
			`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes, created_at, uploaded_at)
			 VALUES ($1, $2, 'f.pdf', 'application/pdf', 1024, $3, %s)`, uploadedAt)
		if _, err := pool.Exec(ctx, q, f.productID, key, createdAt); err != nil {
			t.Fatalf("insert file %s: %v", key, err)
		}
	}

	abandoned := "abandoned/" + uuid.NewString()
	insert(abandoned, false, old)                          // never confirmed, old  -> swept
	insert("confirmed/"+uuid.NewString(), true, old)       // confirmed, old        -> kept
	insert("pending/"+uuid.NewString(), false, time.Now()) // never confirmed, new  -> kept, still uploading

	// The job is global, so it may also sweep genuinely abandoned rows left by
	// other work on this database. Assertions are therefore scoped to this
	// fixture rather than to a table-wide count.
	if _, err := c.SweepAbandonedUploads(ctx); err != nil {
		t.Fatalf("SweepAbandonedUploads: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_files WHERE product_id = $1`, f.productID).Scan(&remaining); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if remaining != 2 {
		t.Errorf("remaining files = %d, want 2", remaining)
	}
	// The storage object has to go too, or the bucket accumulates garbage no
	// row points at.
	var sawAbandoned bool
	for _, key := range deleted {
		if key == abandoned {
			sawAbandoned = true
		}
	}
	if !sawAbandoned {
		t.Errorf("deleted objects = %v, want it to include %s", deleted, abandoned)
	}
}

// A storage failure must not stop the database rows being cleaned, or nothing
// is ever cleaned once one object misbehaves.
func TestSweepContinuesWhenObjectDeleteFails(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	c := NewCleanup(pool)
	c.DeleteObject = func(context.Context, string) error { return fmt.Errorf("s3 unavailable") }

	if _, err := pool.Exec(ctx,
		`INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes, created_at)
		 VALUES ($1, $2, 'f.pdf', 'application/pdf', 1024, $3)`,
		f.productID, "k/"+uuid.NewString(), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	n, err := c.SweepAbandonedUploads(ctx)
	if err != nil {
		t.Fatalf("SweepAbandonedUploads returned an error: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d, want 1 — the row should be gone even though S3 failed", n)
	}
}

func TestDeleteExpiredAnalyticsRespectsTheWindow(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	c := NewCleanup(pool)
	c.RetentionDays = 90

	insertEvent := func(at time.Time) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO events (profile_id, event_type, occurred_at) VALUES ($1, 'profile_view', $2)`,
			f.profileID, at); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	insertVisitor := func(day time.Time) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO visitors (profile_id, visitor_hash, seen_on)
			 VALUES ($1, encode(sha256($2::bytea), 'hex'), $3)`,
			f.profileID, uuid.NewString(), day); err != nil {
			t.Fatalf("insert visitor: %v", err)
		}
	}

	insertEvent(time.Now().Add(-100 * 24 * time.Hour)) // expired
	insertEvent(time.Now().Add(-10 * 24 * time.Hour))  // kept
	insertVisitor(time.Now().Add(-100 * 24 * time.Hour))
	insertVisitor(time.Now().Add(-10 * 24 * time.Hour))

	if _, err := c.DeleteExpiredAnalytics(ctx); err != nil {
		t.Fatalf("DeleteExpiredAnalytics: %v", err)
	}

	var events, visitors int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE profile_id = $1`, f.profileID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM visitors WHERE profile_id = $1`, f.profileID).Scan(&visitors); err != nil {
		t.Fatalf("count visitors: %v", err)
	}

	if events != 1 {
		t.Errorf("events = %d, want 1 (the recent one kept)", events)
	}
	if visitors != 1 {
		t.Errorf("visitors = %d, want 1", visitors)
	}
}

// Cancelling mid-run must stop at a batch boundary with everything already done
// committed — not roll back, and not keep working.
func TestBatchedStopsOnContextCancel(t *testing.T) {
	pool := testPool(t)
	c := NewCleanup(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	n, err := c.batched(ctx, "test", func(context.Context) (int64, error) {
		calls++
		return batchSize, nil // always "full", so only cancellation can stop it
	})
	if err != nil {
		t.Fatalf("batched: %v", err)
	}
	if calls != 0 || n != 0 {
		t.Errorf("ran %d batches after cancellation, want 0", calls)
	}
}

// --- the distributed lock ---------------------------------------------------

func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skipping Redis-backed test")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable (%v); skipping", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// The reason the lock exists: three API instances, one job, one run.
func TestOnlyOneRunnerExecutesAJob(t *testing.T) {
	rdb := testRedis(t)
	name := "test-job-" + uuid.NewString()[:8]
	t.Cleanup(func() { _ = rdb.Del(context.Background(), lockKey(name)).Err() })

	var mu sync.Mutex
	runs := 0
	started := make(chan struct{})
	release := make(chan struct{})

	job := Job{
		Name: name, Interval: time.Hour, Timeout: 30 * time.Second,
		Run: func(context.Context) (int64, error) {
			mu.Lock()
			runs++
			first := runs == 1
			mu.Unlock()

			if first {
				// Hold the lock while the other instances try.
				close(started)
				<-release
			}
			return 0, nil
		},
	}

	// Instance one takes the lock and holds it.
	go New(rdb).runOnce(context.Background(), job)
	<-started

	// Two more instances try while it is held.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			New(rdb).runOnce(context.Background(), job)
		}()
	}
	wg.Wait()

	mu.Lock()
	got := runs
	mu.Unlock()
	if got != 1 {
		t.Errorf("job ran %d times concurrently, want 1", got)
	}

	close(release)
}

// After a run finishes, the lock must be released rather than left to expire —
// otherwise the next scheduled run is skipped for no reason.
func TestLockIsReleasedAfterRun(t *testing.T) {
	rdb := testRedis(t)
	name := "test-job-" + uuid.NewString()[:8]
	ctx := context.Background()
	t.Cleanup(func() { _ = rdb.Del(ctx, lockKey(name)).Err() })

	runs := 0
	job := Job{
		Name: name, Interval: time.Hour, Timeout: 5 * time.Second,
		Run: func(context.Context) (int64, error) { runs++; return 0, nil },
	}

	r := New(rdb)
	r.runOnce(ctx, job)

	if n, _ := rdb.Exists(ctx, lockKey(name)).Result(); n != 0 {
		t.Error("lock survived a completed run")
	}

	r.runOnce(ctx, job)
	if runs != 2 {
		t.Errorf("runs = %d, want 2 — the second run was blocked by a stale lock", runs)
	}
}

// With no Redis there is no lock, and the work still has to happen: a
// single-instance deployment must not stop cleaning up because a cache is
// absent.
func TestRunsWithoutRedis(t *testing.T) {
	runs := 0
	r := New(nil)
	r.runOnce(context.Background(), Job{
		Name: "no-redis", Timeout: time.Second,
		Run: func(context.Context) (int64, error) { runs++; return 0, nil },
	})
	if runs != 1 {
		t.Errorf("runs = %d, want 1", runs)
	}
}
