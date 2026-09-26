package storefront

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/mudit/creatorflow/backend/internal/cache"
)

// Cache-aside is only worth having if a hit avoids the database and a write
// drops the entry, so these tests assert exactly those two things — against a
// real Redis and a real PostgreSQL, because a fake of either would just agree
// with itself.

func testInfra(t *testing.T) (*pgxpool.Pool, *redis.Client) {
	t.Helper()

	dbURL, redisURL := os.Getenv("DATABASE_URL"), os.Getenv("REDIS_URL")
	if dbURL == "" || redisURL == "" {
		t.Skip("DATABASE_URL or REDIS_URL not set; skipping infrastructure-backed test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres unreachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable (%v); skipping", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	return pool, rdb
}

type creatorFixture struct {
	userID    uuid.UUID
	profileID uuid.UUID
	productID uuid.UUID
	username  string
	slug      string
}

func newCreator(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) creatorFixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	f := creatorFixture{username: "sftest" + suffix, slug: "p-" + suffix}

	must := func(step string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("user", pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("sf-test-%s@test.local", suffix)).Scan(&f.userID))
	must("profile", pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name, is_published)
		 VALUES ($1, $2, 'Original Name', true) RETURNING id`,
		f.userID, f.username).Scan(&f.profileID))
	must("product", pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor, status, published_at)
		 VALUES ($1, $2, 'Plan', 49900, 'published', now()) RETURNING id`,
		f.profileID, f.slug).Scan(&f.productID))

	t.Cleanup(func() {
		c := context.Background()
		_, _ = rdb.Del(c, pageKey(f.username), productKey(f.username, f.slug)).Result()
		_, _ = pool.Exec(c, `DELETE FROM products WHERE id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

// The point of the cache: a second read is served without touching the
// database. Proven by changing the row underneath and seeing the old answer.
func TestPageIsServedFromCacheOnSecondRead(t *testing.T) {
	pool, rdb := testInfra(t)
	f := newCreator(t, pool, rdb)
	svc := NewService(NewRepository(pool), cache.New(rdb))
	ctx := context.Background()

	first, err := svc.Page(ctx, f.username)
	if err != nil {
		t.Fatalf("first Page: %v", err)
	}
	if first.Creator.DisplayName != "Original Name" {
		t.Fatalf("display name = %q, want Original Name", first.Creator.DisplayName)
	}

	// Change the database directly, bypassing the service and its invalidation.
	if _, err := pool.Exec(ctx,
		`UPDATE profiles SET display_name = 'Changed Behind The Cache' WHERE id = $1`, f.profileID); err != nil {
		t.Fatalf("update profile: %v", err)
	}

	second, err := svc.Page(ctx, f.username)
	if err != nil {
		t.Fatalf("second Page: %v", err)
	}
	if second.Creator.DisplayName != "Original Name" {
		t.Errorf("display name = %q, want the cached value — the read hit the database",
			second.Creator.DisplayName)
	}
}

// And the other half: invalidation makes the next read see the new state.
func TestInvalidateProfileDropsTheCachedPage(t *testing.T) {
	pool, rdb := testInfra(t)
	f := newCreator(t, pool, rdb)
	svc := NewService(NewRepository(pool), cache.New(rdb))
	ctx := context.Background()

	if _, err := svc.Page(ctx, f.username); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE profiles SET display_name = 'New Name' WHERE id = $1`, f.profileID); err != nil {
		t.Fatalf("update profile: %v", err)
	}

	svc.InvalidateProfile(ctx, f.username)

	after, err := svc.Page(ctx, f.username)
	if err != nil {
		t.Fatalf("Page after invalidation: %v", err)
	}
	if after.Creator.DisplayName != "New Name" {
		t.Errorf("display name = %q, want New Name after invalidation", after.Creator.DisplayName)
	}
}

func TestInvalidateProductDropsBothPages(t *testing.T) {
	pool, rdb := testInfra(t)
	f := newCreator(t, pool, rdb)
	svc := NewService(NewRepository(pool), cache.New(rdb))
	ctx := context.Background()

	if _, err := svc.Product(ctx, f.username, f.slug); err != nil {
		t.Fatalf("warm product: %v", err)
	}
	if _, err := svc.Page(ctx, f.username); err != nil {
		t.Fatalf("warm page: %v", err)
	}

	svc.InvalidateProduct(ctx, f.username, f.slug)

	// A price change is visible on both the product page and the card that
	// lists it, so both keys have to go.
	for _, key := range []string{productKey(f.username, f.slug), pageKey(f.username)} {
		n, err := rdb.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("EXISTS %s: %v", key, err)
		}
		if n != 0 {
			t.Errorf("%s survived invalidation", key)
		}
	}
}

// Case folding matters: /@Rahul and /@rahul are the same page, and caching them
// under different keys would double the memory and halve the hit rate.
func TestCacheKeyIsCaseInsensitive(t *testing.T) {
	pool, rdb := testInfra(t)
	f := newCreator(t, pool, rdb)
	svc := NewService(NewRepository(pool), cache.New(rdb))
	ctx := context.Background()

	if _, err := svc.Page(ctx, f.username); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	shouty := ""
	for _, r := range f.username {
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		shouty += string(r)
	}

	// Invalidating via the uppercase spelling must clear the same entry.
	svc.InvalidateProfile(ctx, shouty)

	n, err := rdb.Exists(ctx, pageKey(f.username)).Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	if n != 0 {
		t.Error("uppercase invalidation missed the lowercase key")
	}
}

// A miss is not cached. Otherwise anyone could fill Redis by walking random
// handles, which is a denial-of-service with no authentication required.
func TestMissesAreNotCached(t *testing.T) {
	pool, rdb := testInfra(t)
	svc := NewService(NewRepository(pool), cache.New(rdb))
	ctx := context.Background()

	unknown := "nosuch" + uuid.NewString()[:8]
	if _, err := svc.Page(ctx, unknown); err == nil {
		t.Fatal("expected ErrNotFound for an unknown creator")
	}

	n, err := rdb.Exists(ctx, pageKey(unknown)).Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	if n != 0 {
		t.Error("a 404 was cached; random handles could fill Redis")
	}
}

// With no cache configured the service must behave exactly as it did before —
// this is the path tests and any Redis-less deployment take.
func TestServiceWorksWithoutCache(t *testing.T) {
	pool, rdb := testInfra(t)
	f := newCreator(t, pool, rdb)
	svc := NewService(NewRepository(pool), cache.New(nil))
	ctx := context.Background()

	page, err := svc.Page(ctx, f.username)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.Creator.Username != f.username {
		t.Errorf("username = %q, want %q", page.Creator.Username, f.username)
	}
	// Nothing should have been written.
	if n, _ := rdb.Exists(ctx, pageKey(f.username)).Result(); n != 0 {
		t.Error("a disabled cache wrote an entry")
	}
}

// A Redis that is down must produce misses, not errors: PostgreSQL is the
// source of truth and the page has to keep rendering without it.
func TestUnreachableRedisDegradesToDatabase(t *testing.T) {
	pool, rdb := testInfra(t)
	f := newCreator(t, pool, rdb)

	dead := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1,
	})
	defer func() { _ = dead.Close() }()

	svc := NewService(NewRepository(pool), cache.New(dead))

	page, err := svc.Page(context.Background(), f.username)
	if err != nil {
		t.Fatalf("Page with Redis down: %v — the storefront must survive a cache outage", err)
	}
	if page.Creator.Username != f.username {
		t.Errorf("username = %q, want %q", page.Creator.Username, f.username)
	}
}
