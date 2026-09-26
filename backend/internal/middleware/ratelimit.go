package middleware

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mudit/creatorflow/backend/internal/httpx"
)

// incrementAndExpire counts one hit and returns the count with the window's
// remaining life.
//
// A Lua script rather than INCR followed by EXPIRE from Go. Two commands means
// two round trips and, worse, a window where the process can die between them —
// leaving a counter with no TTL, which never resets and locks the caller out
// permanently. Redis runs a script atomically, so the key always leaves this
// call with an expiry attached.
var incrementAndExpire = redis.NewScript(`
	local hits = redis.call('INCR', KEYS[1])
	if hits == 1 then
		redis.call('PEXPIRE', KEYS[1], ARGV[1])
	end
	return {hits, redis.call('PTTL', KEYS[1])}
`)

// RateLimitConfig describes one bucket.
type RateLimitConfig struct {
	// Name separates buckets that share an identifier. Without it, a login
	// attempt and a checkout from the same IP would consume the same allowance.
	Name   string
	Limit  int
	Window time.Duration

	// By identifies the caller. IP for anonymous endpoints, user id where a
	// token exists — rate limiting a logged-in creator by IP would punish
	// everyone behind one office connection for the busiest person there.
	By func(c *fiber.Ctx) string
}

// RateLimiter issues middleware backed by Redis.
//
// Fixed-window counters, not a sliding log. The cost is a boundary burst: a
// caller can spend a full window at 11:59:59 and another at 12:00:00, so the
// true worst case is twice the limit over two seconds. For protecting a
// checkout endpoint from a script that is the difference between 10 and 20
// requests, which is not the difference that matters — and the sliding
// alternative stores a sorted set per caller and costs memory and CPU
// proportional to traffic, which is a real price for precision nobody here
// needs. If a limit ever has to be exact, this is the line to revisit.
type RateLimiter struct {
	client *redis.Client
}

func NewRateLimiter(client *redis.Client) *RateLimiter {
	return &RateLimiter{client: client}
}

// Limit returns middleware enforcing one bucket.
//
// It fails OPEN. If Redis is unreachable the request is allowed through, with a
// log line. That is deliberate and worth defending: a rate limiter is a
// protection, not an authorization check, and failing closed would convert a
// cache outage into a total outage of checkout — turning the component that is
// allowed to be down into the one that takes everything with it. The exposure
// while Redis is down is the unprotected behaviour we had before this existed.
func (rl *RateLimiter) Limit(cfg RateLimitConfig) fiber.Handler {
	windowMillis := strconv.FormatInt(cfg.Window.Milliseconds(), 10)
	limitHeader := strconv.Itoa(cfg.Limit)

	return func(c *fiber.Ctx) error {
		if rl.client == nil {
			return c.Next()
		}

		identity := cfg.By(c)
		if identity == "" {
			// No identity means no bucket to charge. Letting it through beats
			// sharing one bucket among every unidentifiable caller, which would
			// let one of them lock out the rest.
			return c.Next()
		}

		key := "ratelimit:" + cfg.Name + ":" + identity

		// A short timeout of its own: the limiter must never be the slowest part
		// of a request, and a Redis that has stopped answering should be treated
		// as absent rather than waited on.
		ctx, cancel := context.WithTimeout(c.Context(), 500*time.Millisecond)
		defer cancel()

		res, err := incrementAndExpire.Run(ctx, rl.client, []string{key}, windowMillis).Int64Slice()
		if err != nil || len(res) != 2 {
			slog.Warn("rate limiter unavailable, allowing request",
				"error", err, "bucket", cfg.Name, "request_id", FromContext(c))
			return c.Next()
		}

		hits, ttlMillis := res[0], res[1]
		remaining := int64(cfg.Limit) - hits
		if remaining < 0 {
			remaining = 0
		}

		// Standard-ish headers, so a well-behaved client can slow itself down
		// before being refused rather than discovering the limit by hitting it.
		c.Set("X-RateLimit-Limit", limitHeader)
		c.Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
		c.Set("X-RateLimit-Reset", strconv.FormatInt(int64(time.Now().Add(time.Duration(ttlMillis)*time.Millisecond).Unix()), 10))

		if hits > int64(cfg.Limit) {
			retryAfter := (ttlMillis + 999) / 1000 // round up: 0 would invite an immediate retry
			c.Set("Retry-After", strconv.FormatInt(retryAfter, 10))

			slog.Info("rate limited",
				"bucket", cfg.Name, "hits", hits, "limit", cfg.Limit,
				"request_id", FromContext(c), "path", c.Path())

			return httpx.ErrRateLimited
		}

		return c.Next()
	}
}

// ByIP buckets anonymous callers.
//
// c.IP() is the peer address unless Fiber is told to trust a proxy header. That
// default is the safe one: a trusted X-Forwarded-For is a header any client can
// forge, so honouring it without a trusted proxy in front would let one script
// bypass every limit here by inventing a new address per request. Phase 15 sets
// the proxy config when there is a real load balancer to trust.
func ByIP(c *fiber.Ctx) string { return c.IP() }

// ByUser buckets authenticated callers, falling back to the address for
// requests that reach a limiter before the token is parsed.
func ByUser(c *fiber.Ctx) string {
	if id, ok := UserID(c); ok {
		return id.String()
	}
	return c.IP()
}

// ByPathParam buckets on something in the URL — used for the download endpoint,
// where the interesting question is how many guesses have been made against one
// order, not how many a single address has made in total.
func ByPathParam(name string) func(*fiber.Ctx) string {
	return func(c *fiber.Ctx) string {
		return c.Params(name)
	}
}
