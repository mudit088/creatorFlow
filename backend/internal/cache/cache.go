// Package cache is a thin, typed layer over Redis for cache-aside reads.
//
// Every operation here degrades rather than fails. Redis is not a source of
// truth in this system — PostgreSQL is — so a cache that is down, slow or
// returning nonsense must produce a cache miss, never an error the caller has
// to handle. That property is what lets a call site read as "try the cache,
// otherwise do the real work" with no error branch for the cache itself.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type Client struct {
	rdb *redis.Client
}

// New accepts a nil client, which disables caching entirely. That is what tests
// and any deployment without Redis use, and it means the disabled path is the
// same code path rather than a special case at every call site.
func New(rdb *redis.Client) *Client {
	return &Client{rdb: rdb}
}

func (c *Client) enabled() bool { return c != nil && c.rdb != nil }

// opTimeout bounds every cache call. A cache exists to be faster than the thing
// it is caching; one that takes longer than the database is worse than useless,
// so a slow Redis is treated as a miss and the query runs instead.
const opTimeout = 200 * time.Millisecond

// GetJSON reports whether the key was found and decoded into dst.
//
// A decode failure is a miss, not an error. It means the cached shape predates
// a code change, and the correct response is to ignore it and recompute — which
// is also why keys carry a version prefix: the two mechanisms together mean a
// deploy can never be poisoned by what an older build wrote.
func (c *Client) GetJSON(ctx context.Context, key string, dst any) bool {
	if !c.enabled() {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	raw, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false
	}
	if err != nil {
		slog.Debug("cache read failed, treating as miss", "key", key, "error", err)
		return false
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		slog.Warn("cached value could not be decoded, ignoring", "key", key, "error", err)
		return false
	}
	return true
}

// SetJSON stores a value, best effort. The caller already has the answer it
// needs, so a failure to remember it is not worth reporting upward.
func (c *Client) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) {
	if !c.enabled() {
		return
	}

	raw, err := json.Marshal(value)
	if err != nil {
		slog.Warn("value could not be encoded for cache", "key", key, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	if err := c.rdb.Set(ctx, key, raw, ttl).Err(); err != nil {
		slog.Debug("cache write failed", "key", key, "error", err)
	}
}

// Delete removes keys. Used for invalidation on write.
//
// A failed delete is the one degradation with real consequences: the cache
// keeps serving a stale page until its TTL expires. That is why entries carry a
// short TTL as well — invalidation is the fast path, expiry is the guarantee.
func (c *Client) Delete(ctx context.Context, keys ...string) {
	if !c.enabled() || len(keys) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
		slog.Warn("cache invalidation failed; entries will serve stale until they expire",
			"keys", keys, "error", err)
	}
}
