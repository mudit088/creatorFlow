package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// batchSize bounds every delete below.
//
// One statement deleting a million rows takes a long lock, writes a huge WAL
// record, and cannot be interrupted — so a cleanup job that has fallen behind
// becomes an outage the first time it runs. Batching turns that into many short
// statements that interleave with request traffic and can stop between batches
// when the process is shutting down.
const batchSize = 5000

// maxBatches stops a single run from working indefinitely. Whatever is left is
// picked up on the next interval, which keeps each run's cost predictable.
const maxBatches = 20

type Cleanup struct {
	pool *pgxpool.Pool

	// RetentionDays is how long raw analytics rows are kept. The aggregates a
	// creator sees are computed from them, so this is the one setting that
	// decides how far back a dashboard can look.
	RetentionDays int

	// PendingOrderTTL is how long an unpaid order stays pending before it is
	// cancelled. Long enough that a slow bank redirect is not cut off, short
	// enough that the orders table is not mostly abandoned checkouts.
	PendingOrderTTL time.Duration

	// AbandonedUploadTTL is how long a file row may exist without its upload
	// being confirmed.
	AbandonedUploadTTL time.Duration

	// DeleteObject removes the storage object behind an abandoned upload. A
	// function rather than the storage client, so this package does not depend
	// on S3 to be tested.
	DeleteObject func(ctx context.Context, key string) error
}

func NewCleanup(pool *pgxpool.Pool) *Cleanup {
	return &Cleanup{
		pool:               pool,
		RetentionDays:      90,
		PendingOrderTTL:    24 * time.Hour,
		AbandonedUploadTTL: 24 * time.Hour,
	}
}

// ExpirePendingOrders cancels checkouts nobody completed.
//
// Cancelled, not deleted. An abandoned order is evidence — of a buyer who tried
// and failed, of a payment provider that timed out — and a creator asking "did
// someone try to buy this?" deserves an answer. It also keeps the row available
// if a very late webhook arrives, which then lands on a cancelled order and is
// recorded as a refusal rather than silently creating one.
//
// The status filter matters twice over: it is the partial index the query uses,
// and it means a row that was paid between the SELECT and the UPDATE is left
// alone, because the UPDATE re-checks the status it is changing.
func (c *Cleanup) ExpirePendingOrders(ctx context.Context) (int64, error) {
	const q = `
		UPDATE orders SET status = 'cancelled'
		WHERE id IN (
		    SELECT id FROM orders
		    WHERE status = 'pending' AND created_at < now() - $1::interval
		    ORDER BY created_at
		    LIMIT $2
		)
		AND status = 'pending'`

	return c.batched(ctx, "expire_pending_orders", func(ctx context.Context) (int64, error) {
		tag, err := c.pool.Exec(ctx, q, intervalOf(c.PendingOrderTTL), batchSize)
		if err != nil {
			return 0, fmt.Errorf("expire pending orders: %w", err)
		}
		return tag.RowsAffected(), nil
	})
}

// SweepAbandonedUploads removes file rows whose upload was never confirmed.
//
// The row is deleted first and the object second, which is the same ordering
// the product service uses when a creator deletes a file, and for the same
// reason: a row pointing at bytes that do not exist is a lie the system will act
// on, while an object with no row is wasted storage that can be swept later.
//
// Most of these objects do not exist at all — the row is created when a
// presigned URL is issued, and an abandoned upload is one where the browser
// never sent anything. Deleting a missing object is a no-op in S3, so this does
// not need to distinguish the two cases.
func (c *Cleanup) SweepAbandonedUploads(ctx context.Context) (int64, error) {
	const q = `
		DELETE FROM product_files
		WHERE id IN (
		    SELECT id FROM product_files
		    WHERE uploaded_at IS NULL AND created_at < now() - $1::interval
		    ORDER BY created_at
		    LIMIT $2
		)
		RETURNING s3_key`

	return c.batched(ctx, "sweep_abandoned_uploads", func(ctx context.Context) (int64, error) {
		rows, err := c.pool.Query(ctx, q, intervalOf(c.AbandonedUploadTTL), batchSize)
		if err != nil {
			return 0, fmt.Errorf("sweep abandoned uploads: %w", err)
		}

		var keys []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan abandoned upload: %w", err)
			}
			keys = append(keys, key)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}

		// Storage is cleaned after the rows are committed. A failure here leaves
		// an orphaned object, which costs pennies and is recoverable; failing the
		// whole batch over it would mean the rows are never cleaned either.
		if c.DeleteObject != nil {
			for _, key := range keys {
				if err := c.DeleteObject(ctx, key); err != nil {
					slog.Warn("abandoned upload row deleted but object remains",
						"key", key, "error", err)
				}
			}
		}
		return int64(len(keys)), nil
	})
}

// DeleteExpiredAnalytics enforces the retention window migration 008 promised.
//
// Events go first, then visitors: events reference visitors, and deleting the
// pseudonym while an event still points at it would either fail or null out the
// link depending on the foreign key. Doing it in dependency order means neither.
//
// This is the only place raw analytics is removed, and it is what makes the
// privacy claim in migration 008 true rather than aspirational: without it, the
// daily-rotating hashes still accumulate forever, and a long enough history of
// pseudonyms is itself a record worth not keeping.
func (c *Cleanup) DeleteExpiredAnalytics(ctx context.Context) (int64, error) {
	const deleteEvents = `
		DELETE FROM events
		WHERE id IN (
		    SELECT id FROM events
		    WHERE occurred_at < now() - $1::interval
		    ORDER BY occurred_at
		    LIMIT $2
		)`

	const deleteVisitors = `
		DELETE FROM visitors
		WHERE id IN (
		    SELECT id FROM visitors
		    WHERE seen_on < (now() - $1::interval)::date
		    ORDER BY seen_on
		    LIMIT $2
		)`

	window := fmt.Sprintf("%d days", c.RetentionDays)

	events, err := c.batched(ctx, "delete_expired_events", func(ctx context.Context) (int64, error) {
		tag, err := c.pool.Exec(ctx, deleteEvents, window, batchSize)
		if err != nil {
			return 0, fmt.Errorf("delete expired events: %w", err)
		}
		return tag.RowsAffected(), nil
	})
	if err != nil {
		return events, err
	}

	visitors, err := c.batched(ctx, "delete_expired_visitors", func(ctx context.Context) (int64, error) {
		tag, err := c.pool.Exec(ctx, deleteVisitors, window, batchSize)
		if err != nil {
			return 0, fmt.Errorf("delete expired visitors: %w", err)
		}
		return tag.RowsAffected(), nil
	})
	return events + visitors, err
}

// batched repeats a delete until it stops finding rows, the batch cap is hit, or
// the context ends.
//
// The context check between batches is what makes shutdown clean: a job that has
// been running for a while stops at a batch boundary with everything it has done
// already committed, rather than being killed mid-statement.
func (c *Cleanup) batched(ctx context.Context, name string, step func(context.Context) (int64, error)) (int64, error) {
	var total int64

	for i := 0; i < maxBatches; i++ {
		select {
		case <-ctx.Done():
			return total, nil
		default:
		}

		n, err := step(ctx)
		if err != nil {
			return total, err
		}
		total += n

		// A short batch means there is nothing left to find.
		if n < batchSize {
			return total, nil
		}
	}

	slog.Info("cleanup hit its batch cap; remaining rows deferred to the next run",
		"job", name, "deleted", total)
	return total, nil
}

// intervalOf renders a duration for Postgres. Seconds, because a duration in Go
// has no notion of months and neither should this.
func intervalOf(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
