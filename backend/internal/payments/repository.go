package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/database"
)

type Repository struct {
	db   database.Querier
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool, pool: pool}
}

func (r *Repository) InTx(ctx context.Context, fn func(*Repository) error) error {
	return database.RunInTx(ctx, r.pool, func(q database.Querier) error {
		return fn(&Repository{db: q, pool: r.pool})
	})
}

// errDuplicateEvent means this event id is already in the table. The caller
// treats it as "already handled" and acknowledges without doing the work twice.
var errDuplicateEvent = errors.New("webhook event already recorded")

// RecordWebhook claims an event for processing.
//
// This is the delivery-level idempotency guard, and it runs inside the same
// transaction as the work it guards. That placement is the entire design:
//
//   - Recorded in a separate transaction, a crash midway through applying the
//     event would leave the row committed. The retry would then see a duplicate,
//     skip the work, and the payment would be lost with a webhook row claiming
//     it was received.
//   - Inside one transaction, a failure rolls the claim back too, so Razorpay's
//     retry re-claims and re-applies.
//
// Concurrency falls out of the same property. Two simultaneous deliveries of one
// event both reach this INSERT; Postgres makes the second wait on the unique
// index until the first transaction ends. If the first committed, the second
// conflicts and does nothing. If the first rolled back, the second proceeds.
// Neither outcome requires a lock we manage ourselves.
func (r *Repository) RecordWebhook(ctx context.Context, eventID, eventType string, rawPayload []byte, signatureValid bool) (uuid.UUID, error) {
	const q = `
		INSERT INTO payment_webhooks (provider_event_id, event_type, raw_payload, signature_valid)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, provider_event_id) DO NOTHING
		RETURNING id`

	var id uuid.UUID
	err := r.db.QueryRow(ctx, q, eventID, eventType, rawPayload, signatureValid).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errDuplicateEvent
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("record webhook: %w", err)
	}
	return id, nil
}

// MarkWebhookProcessed closes out the row. processError is nil on success; when
// set, the row stays visible as something a human should look at, and the
// partial index on processed_at IS NULL keeps that query cheap.
func (r *Repository) MarkWebhookProcessed(ctx context.Context, webhookID uuid.UUID, processError *string) error {
	const q = `UPDATE payment_webhooks SET processed_at = now(), process_error = $2 WHERE id = $1`

	if _, err := r.db.Exec(ctx, q, webhookID, processError); err != nil {
		return fmt.Errorf("mark webhook processed: %w", err)
	}
	return nil
}

// FindOrderByProviderOrderID resolves Razorpay's order id to ours.
//
// FOR UPDATE locks the row for the rest of the transaction. Without it, two
// events for the same order — a capture and an order.paid arriving together —
// could each read status 'pending' and both decide they are the one to settle
// it. The lock makes the second wait and then see the truth.
func (r *Repository) FindOrderByProviderOrderID(ctx context.Context, providerOrderID string) (*paidOrder, error) {
	const q = `
		SELECT id, status, total_minor, currency, buyer_email
		FROM orders WHERE provider_order_id = $1
		FOR UPDATE`

	var o paidOrder
	err := r.db.QueryRow(ctx, q, providerOrderID).Scan(&o.ID, &o.Status, &o.TotalMinor, &o.Currency, &o.BuyerEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find order by provider order id: %w", err)
	}
	return &o, nil
}

var errOrderNotFound = errors.New("no order matches this provider order id")

// RecordPayment writes the attempt. ON CONFLICT DO NOTHING against the unique
// provider_payment_id means a replayed capture cannot become a second payment
// row — the money-level guard, independent of the event-level one above.
func (r *Repository) RecordPayment(ctx context.Context, orderID uuid.UUID, providerPaymentID, status string, amountMinor int64, currency string, errCode, errDescription *string, captured bool) error {
	const q = `
		INSERT INTO payments (order_id, provider_payment_id, status, amount_minor, currency,
		                      error_code, error_description, captured_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, CASE WHEN $8 THEN now() ELSE NULL END)
		ON CONFLICT (provider_payment_id) DO NOTHING`

	if _, err := r.db.Exec(ctx, q, orderID, providerPaymentID, status, amountMinor, currency, errCode, errDescription, captured); err != nil {
		return fmt.Errorf("record payment: %w", err)
	}
	return nil
}

// MarkOrderPaid moves pending to paid, and only pending.
//
// The status check lives in the WHERE clause rather than in Go. A refunded order
// must not be quietly re-marked paid by a late duplicate event, and expressing
// that as "update the row if it is still pending" is one atomic statement
// instead of a read, a decision and a write that another transaction can
// interleave with.
func (r *Repository) MarkOrderPaid(ctx context.Context, orderID uuid.UUID) (bool, error) {
	const q = `UPDATE orders SET status = 'paid', paid_at = now() WHERE id = $1 AND status = 'pending'`

	tag, err := r.db.Exec(ctx, q, orderID)
	if err != nil {
		return false, fmt.Errorf("mark order paid: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GrantEntitlements creates the right to download every line of the order.
//
// One statement, derived from the order itself: the buyer email and the product
// ids come from rows the database already holds, so nothing a webhook said can
// influence who gets access to what. ON CONFLICT makes it safe to run again.
func (r *Repository) GrantEntitlements(ctx context.Context, orderID uuid.UUID) (int64, error) {
	const q = `
		INSERT INTO entitlements (order_id, product_id, buyer_email)
		SELECT i.order_id, i.product_id, o.buyer_email
		FROM order_items i
		JOIN orders o ON o.id = i.order_id
		WHERE i.order_id = $1
		ON CONFLICT (order_id, product_id) DO NOTHING`

	tag, err := r.db.Exec(ctx, q, orderID)
	if err != nil {
		return 0, fmt.Errorf("grant entitlements: %w", err)
	}
	return tag.RowsAffected(), nil
}
