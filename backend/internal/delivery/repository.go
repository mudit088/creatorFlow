package delivery

import (
	"context"
	"fmt"

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

// FindDeliverables answers one question: may this email download anything for
// this order, and if so, what?
//
// Access is decided by the entitlements table alone. It is not re-derived by
// walking orders and joining order_items, because that logic would then exist in
// two places — here and in the webhook that grants it — and two copies of an
// authorization rule eventually disagree. The webhook decides who is entitled;
// this query only reads the answer.
//
// Which is also why orders.status is not checked here. A refund sets
// revoked_at on the entitlement, so the entitlement already carries the verdict,
// and adding a second condition would quietly create a second rule.
//
// buyer_email is citext, so Buyer@Example.com and buyer@example.com are the same
// buyer — a case difference between checkout and the download form must not cost
// someone the thing they paid for.
func (r *Repository) FindDeliverables(ctx context.Context, orderID uuid.UUID, buyerEmail string) ([]Deliverable, error) {
	const q = `
		SELECT f.id, e.product_id, p.title, f.s3_key, f.original_name, f.content_type, f.size_bytes
		FROM entitlements e
		JOIN products p ON p.id = e.product_id
		-- uploaded_at IS NOT NULL excludes files whose presigned upload was never
		-- confirmed: a row exists from the moment a URL is issued, and handing a
		-- buyer a signed URL to an object that was never stored produces a
		-- confusing 404 from S3 instead of an honest answer from us.
		JOIN product_files f ON f.product_id = e.product_id AND f.uploaded_at IS NOT NULL
		WHERE e.order_id = $1
		  AND e.buyer_email = $2
		  AND e.revoked_at IS NULL
		ORDER BY p.title, f.original_name`

	rows, err := r.db.Query(ctx, q, orderID, buyerEmail)
	if err != nil {
		return nil, fmt.Errorf("find deliverables: %w", err)
	}
	defer rows.Close()

	var out []Deliverable
	for rows.Next() {
		var d Deliverable
		if err := rows.Scan(&d.FileID, &d.ProductID, &d.ProductTitle, &d.S3Key,
			&d.OriginalName, &d.ContentType, &d.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan deliverable: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
