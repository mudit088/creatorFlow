package middleware

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mudit/creatorflow/backend/internal/httpx"
)

// Against a real Redis, because the behaviour being tested is the atomicity of
// the Lua script and the TTL semantics of the key — both of which a fake would
// simply assert into existence.

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

// newApp wires one limited route. Each test uses a unique bucket name so runs
// do not inherit each other's counters.
func newApp(t *testing.T, client *redis.Client, cfg RateLimitConfig) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler, DisableStartupMessage: true})
	app.Use(RequestID())
	app.Get("/limited/:id", NewRateLimiter(client).Limit(cfg), func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	if client != nil {
		t.Cleanup(func() {
			ctx := context.Background()
			keys, _ := client.Keys(ctx, "ratelimit:"+cfg.Name+":*").Result()
			if len(keys) > 0 {
				_ = client.Del(ctx, keys...).Err()
			}
		})
	}
	return app
}

func get(t *testing.T, app *fiber.App, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodGet, path, nil)
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	rec := httptest.NewRecorder()
	rec.Code = resp.StatusCode
	for k, v := range resp.Header {
		rec.Header()[k] = v
	}
	return rec
}

func TestAllowsUpToLimitThenRefuses(t *testing.T) {
	client := testRedis(t)
	name := "test-" + uuid.NewString()[:8]
	app := newApp(t, client, RateLimitConfig{Name: name, Limit: 3, Window: time.Minute, By: ByIP})

	for i := 1; i <= 3; i++ {
		if rec := get(t, app, "/limited/x"); rec.Code != fiber.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rec.Code)
		}
	}

	rec := get(t, app, "/limited/x")
	if rec.Code != fiber.StatusTooManyRequests {
		t.Fatalf("request 4 = %d, want 429", rec.Code)
	}
	// A 429 with no Retry-After invites an immediate retry, which is the thing
	// the limiter is trying to stop.
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 carried no Retry-After header")
	}
}

// The headers exist so a well-behaved client can slow down before being
// refused, rather than discovering the limit by hitting it.
func TestReportsRemainingAllowance(t *testing.T) {
	client := testRedis(t)
	name := "test-" + uuid.NewString()[:8]
	app := newApp(t, client, RateLimitConfig{Name: name, Limit: 5, Window: time.Minute, By: ByIP})

	first := get(t, app, "/limited/x")
	if got := first.Header().Get("X-RateLimit-Remaining"); got != "4" {
		t.Errorf("remaining after 1 of 5 = %q, want \"4\"", got)
	}
	if got := first.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Errorf("limit header = %q, want \"5\"", got)
	}

	second := get(t, app, "/limited/x")
	if got := second.Header().Get("X-RateLimit-Remaining"); got != "3" {
		t.Errorf("remaining after 2 of 5 = %q, want \"3\"", got)
	}
}

// Buckets must not bleed into each other: exhausting one identity cannot lock
// out another.
func TestBucketsAreIndependentPerIdentity(t *testing.T) {
	client := testRedis(t)
	name := "test-" + uuid.NewString()[:8]
	app := newApp(t, client, RateLimitConfig{
		Name: name, Limit: 2, Window: time.Minute, By: ByPathParam("id"),
	})

	for i := 0; i < 3; i++ {
		get(t, app, "/limited/order-a")
	}
	if rec := get(t, app, "/limited/order-a"); rec.Code != fiber.StatusTooManyRequests {
		t.Fatalf("order-a = %d, want 429 after exhausting it", rec.Code)
	}

	if rec := get(t, app, "/limited/order-b"); rec.Code != fiber.StatusOK {
		t.Errorf("order-b = %d, want 200 — a different identity has its own allowance", rec.Code)
	}
}

// The window must expire. A counter left without a TTL would lock a caller out
// forever, which is the failure the Lua script exists to prevent.
func TestWindowExpires(t *testing.T) {
	client := testRedis(t)
	name := "test-" + uuid.NewString()[:8]
	app := newApp(t, client, RateLimitConfig{
		Name: name, Limit: 1, Window: 1200 * time.Millisecond, By: ByIP,
	})

	if rec := get(t, app, "/limited/x"); rec.Code != fiber.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	if rec := get(t, app, "/limited/x"); rec.Code != fiber.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", rec.Code)
	}

	time.Sleep(1500 * time.Millisecond)

	if rec := get(t, app, "/limited/x"); rec.Code != fiber.StatusOK {
		t.Errorf("after the window = %d, want 200 — the counter did not expire", rec.Code)
	}
}

// Every key must carry an expiry the moment it is created. Without it a process
// dying between INCR and EXPIRE would leave a permanent counter.
func TestCounterAlwaysHasATTL(t *testing.T) {
	client := testRedis(t)
	name := "test-" + uuid.NewString()[:8]
	app := newApp(t, client, RateLimitConfig{Name: name, Limit: 5, Window: time.Minute, By: ByIP})

	get(t, app, "/limited/x")

	ctx := context.Background()
	keys, err := client.Keys(ctx, "ratelimit:"+name+":*").Result()
	if err != nil || len(keys) == 0 {
		t.Fatalf("no counter key created (err %v)", err)
	}
	ttl, err := client.PTTL(ctx, keys[0]).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("ttl = %v, want a positive expiry on %s", ttl, keys[0])
	}
}

// The property that matters most in an outage: no Redis means no limiting, not
// no service. A limiter that fails closed turns the component allowed to be
// down into the one that takes checkout with it.
func TestFailsOpenWhenRedisIsUnavailable(t *testing.T) {
	// A client pointed at a closed port: every call errors.
	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = dead.Close() }()

	app := newApp(t, dead, RateLimitConfig{
		Name: "test-dead", Limit: 1, Window: time.Minute, By: ByIP,
	})

	for i := 1; i <= 3; i++ {
		if rec := get(t, app, "/limited/x"); rec.Code != fiber.StatusOK {
			t.Fatalf("request %d = %d, want 200 — the limiter must fail open", i, rec.Code)
		}
	}
}

// A nil client is the "limiting disabled" case, used by tests and by any
// deployment without Redis.
func TestNilClientDisablesLimiting(t *testing.T) {
	app := newApp(t, nil, RateLimitConfig{Name: "test-nil", Limit: 1, Window: time.Minute, By: ByIP})

	for i := 0; i < 3; i++ {
		if rec := get(t, app, "/limited/x"); rec.Code != fiber.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, rec.Code)
		}
	}
}

// An identifier the limiter cannot determine must not put every such caller in
// one shared bucket, where any one of them could lock out the rest.
func TestEmptyIdentityIsNotLimited(t *testing.T) {
	client := testRedis(t)
	app := newApp(t, client, RateLimitConfig{
		Name: "test-empty", Limit: 1, Window: time.Minute,
		By: func(*fiber.Ctx) string { return "" },
	})

	for i := 0; i < 3; i++ {
		if rec := get(t, app, fmt.Sprintf("/limited/%d", i)); rec.Code != fiber.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, rec.Code)
		}
	}
}
