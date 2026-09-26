package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/database"
)

type Repository struct {
	db database.Querier
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool}
}

// UpsertVisitor returns the visitor row for this pseudonym today, creating it if
// this is the first time it has been seen.
//
// ON CONFLICT DO UPDATE rather than a SELECT followed by an INSERT: two requests
// from the same browser can arrive at once, and the check-then-insert version
// has both threads find nothing and then one of them fail on the unique index.
// The upsert is one statement and one round trip, and it is also how last_seen_at
// stays current without a second write.
func (r *Repository) UpsertVisitor(ctx context.Context, profileID uuid.UUID, visitorHash string, day time.Time) (uuid.UUID, error) {
	const q = `
		INSERT INTO visitors (profile_id, visitor_hash, seen_on)
		VALUES ($1, $2, $3)
		ON CONFLICT (profile_id, visitor_hash, seen_on)
		DO UPDATE SET last_seen_at = now()
		RETURNING id`

	var id uuid.UUID
	if err := r.db.QueryRow(ctx, q, profileID, visitorHash, day).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("upsert visitor: %w", err)
	}
	return id, nil
}

// InsertEvent appends one row. Nothing here reads back or updates: an event is
// a fact about a moment, and a table that is only ever appended to is one that
// can be partitioned, archived and deleted by date without coordination.
func (r *Repository) InsertEvent(ctx context.Context, profileID uuid.UUID, visitorID *uuid.UUID, eventType string,
	productID, linkID, orderID *uuid.UUID, referrerHost *string, occurredAt time.Time) error {

	const q = `
		INSERT INTO events (profile_id, visitor_id, event_type, product_id, link_id, order_id, referrer_host, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	if _, err := r.db.Exec(ctx, q, profileID, visitorID, eventType, productID, linkID, orderID, referrerHost, occurredAt); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}
