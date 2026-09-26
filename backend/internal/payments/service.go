package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// SignatureVerifier is the one thing this service needs from the payment
// provider. Declared here, so the webhook can be tested by signing payloads with
// a known secret rather than by owning a Razorpay account.
type SignatureVerifier interface {
	VerifyWebhookSignature(body []byte, signature string) bool
}

type Service struct {
	repo     *Repository
	verifier SignatureVerifier
}

func NewService(repo *Repository, verifier SignatureVerifier) *Service {
	return &Service{repo: repo, verifier: verifier}
}

// Handle applies one webhook delivery.
//
// The order of operations is the whole design, so it is worth stating plainly:
//
//  1. Verify the signature over the raw bytes. Nothing below this line runs for
//     a request we cannot attribute to Razorpay.
//  2. Claim the event id and do all the work in ONE transaction, so a crash
//     halfway leaves nothing claimed and the retry re-does it.
//  3. Trust the database for money and identity. The event supplies an amount;
//     the order supplies the amount that is owed. They must match, and the one
//     that wins is ours.
//
// It returns an error only for failures Razorpay should retry. A malformed,
// unmatched or mismatched event is recorded and acknowledged, because retrying
// it would produce the same refusal every few minutes for a day.
func (s *Service) Handle(ctx context.Context, rawBody []byte, signature, eventID string) error {
	if !s.verifier.VerifyWebhookSignature(rawBody, signature) {
		return ErrInvalidSignature
	}

	var event Event
	if err := json.Unmarshal(rawBody, &event); err != nil || event.Event == "" {
		return ErrMalformedEvent
	}

	return s.repo.InTx(ctx, func(tx *Repository) error {
		webhookID, err := tx.RecordWebhook(ctx, eventID, event.Event, rawBody, true)
		if errors.Is(err, errDuplicateEvent) {
			// Already recorded by a delivery that committed. Acknowledge and do
			// nothing: this is Razorpay's retry of something we finished.
			slog.Info("webhook already processed, ignoring replay", "event_id", eventID, "event", event.Event)
			return nil
		}
		if err != nil {
			return err
		}

		result := s.apply(ctx, tx, event)
		if result.transient != nil {
			// Roll back, claim included, so the retry re-applies rather than
			// finding the event already recorded and skipping the work.
			return result.transient
		}
		if result.processError != nil {
			slog.Warn("webhook recorded but not applied",
				"event_id", eventID, "event", event.Event, "reason", *result.processError)
		}
		return tx.MarkWebhookProcessed(ctx, webhookID, result.processError)
	})
}

// apply interprets one event. Anything it returns in outcome.processError is a
// permanent refusal, stored against the webhook row; anything it returns as an
// error is a transient failure that rolls the transaction back so Razorpay can
// retry.
func (s *Service) apply(ctx context.Context, tx *Repository, event Event) outcome {
	entity := event.Payload.Payment.Entity

	switch event.Event {
	case EventPaymentCaptured, EventPaymentFailed:
	case EventOrderPaid:
		// Deliberately not a second settlement path. For single-payment orders
		// order.paid carries no information payment.captured did not, and having
		// two code paths that can both mark an order paid is how the two
		// eventually disagree. Stored, acknowledged, not acted on.
		return refuse("order.paid is recorded for audit only; payment.captured settles orders")
	default:
		return refuse(fmt.Sprintf("event type %q is not handled", event.Event))
	}

	if entity.ID == "" || entity.OrderID == "" {
		return refuse("event carried no payment id or order id")
	}

	order, err := tx.FindOrderByProviderOrderID(ctx, entity.OrderID)
	if errors.Is(err, errOrderNotFound) {
		// Razorpay knows about an order we do not. That is worth a human
		// looking, not a retry loop: it means either a test event from another
		// environment sharing the account, or an order our side lost.
		return refuse(fmt.Sprintf("no local order for provider order %s", entity.OrderID))
	}
	if err != nil {
		return retry(err)
	}

	if event.Event == EventPaymentFailed {
		// A failed attempt is history, not a verdict on the order. The buyer can
		// try a different card against the same pending order, so the order's
		// status is left alone and only the attempt is written down.
		desc := nullable(entity.ErrorDescription)
		code := nullable(entity.ErrorCode)
		if err := tx.RecordPayment(ctx, order.ID, entity.ID, PaymentStatusFailed,
			entity.Amount, orCurrency(entity.Currency, order.Currency), code, desc, false); err != nil {
			return retry(err)
		}
		return applied()
	}

	// The check that makes this endpoint safe to expose.
	//
	// The amount is taken from the order, not the event, and a capture for the
	// wrong amount settles nothing. Without this, anyone who learned a provider
	// order id could pay 1 rupee for a 499 rupee product — assuming they could
	// also forge the signature, which is exactly why neither check alone is
	// enough. Currency is compared for the same reason: 49900 JPY is not 49900
	// paise.
	if entity.Amount != order.TotalMinor {
		return refuse(fmt.Sprintf("amount mismatch: event %d, order %d", entity.Amount, order.TotalMinor))
	}
	if entity.Currency != "" && entity.Currency != order.Currency {
		return refuse(fmt.Sprintf("currency mismatch: event %s, order %s", entity.Currency, order.Currency))
	}

	if err := tx.RecordPayment(ctx, order.ID, entity.ID, PaymentStatusCaptured,
		entity.Amount, order.Currency, nil, nil, true); err != nil {
		return retry(err)
	}

	moved, err := tx.MarkOrderPaid(ctx, order.ID)
	if err != nil {
		return retry(err)
	}
	if !moved && order.Status != "paid" {
		// Not pending and not already paid — refunded or cancelled. A late
		// capture against such an order is a reconciliation problem, and quietly
		// flipping it back to paid would hide it.
		return refuse(fmt.Sprintf("order is %s, not pending; capture not applied", order.Status))
	}

	// Entitlements are granted in the same transaction that marked the order
	// paid. "Paid" and "may download" are one fact, and a system where they can
	// disagree is one where a buyer pays and gets nothing.
	granted, err := tx.GrantEntitlements(ctx, order.ID)
	if err != nil {
		return retry(err)
	}

	// Analytics, in the same transaction. A failure here is logged rather than
	// returned: a sale that was not counted is a reporting gap, while rolling
	// back a settled payment over one would be a catastrophe.
	if err := tx.RecordPurchaseEvents(ctx, order.ID); err != nil {
		slog.Warn("order settled but purchase event not recorded", "order_id", order.ID, "error", err)
	}

	slog.Info("order settled",
		"order_id", order.ID, "payment_id", entity.ID,
		"amount_minor", entity.Amount, "entitlements_granted", granted)
	return applied()
}

func applied() outcome { return outcome{} }

// refuse records why an event was not applied and still acknowledges it.
func refuse(reason string) outcome { return outcome{processError: &reason} }

// retry is a transient failure. Panicking would be wrong and swallowing it would
// lose a payment, so it is carried out as a real error that rolls the
// transaction back and makes Razorpay deliver again.
func retry(err error) outcome {
	reason := err.Error()
	return outcome{processError: &reason, transient: err}
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orCurrency(eventCurrency, orderCurrency string) string {
	if eventCurrency == "" {
		return orderCurrency
	}
	return eventCurrency
}
