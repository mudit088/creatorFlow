package payments

import (
	"errors"

	"github.com/google/uuid"
)

// The events this API acts on. Razorpay sends many more; anything not listed
// here is stored and acknowledged without being interpreted, because silently
// half-handling an event type nobody designed for is worse than ignoring it.
const (
	EventPaymentCaptured = "payment.captured"
	EventPaymentFailed   = "payment.failed"
	EventOrderPaid       = "order.paid"
)

const (
	PaymentStatusCaptured = "captured"
	PaymentStatusFailed   = "failed"
)

// Event is the subset of a Razorpay webhook body this service reads. Everything
// else in the payload is kept verbatim in payment_webhooks.raw_payload rather
// than modelled, so a field we never anticipated is still available later.
type Event struct {
	Event   string `json:"event"`
	Payload struct {
		Payment struct {
			Entity struct {
				ID               string `json:"id"`
				OrderID          string `json:"order_id"`
				Amount           int64  `json:"amount"`
				Currency         string `json:"currency"`
				Status           string `json:"status"`
				ErrorCode        string `json:"error_code"`
				ErrorDescription string `json:"error_description"`
			} `json:"entity"`
		} `json:"payment"`
	} `json:"payload"`
}

// outcome is what happened to one event, recorded against the webhook row.
//
// Note what is not here: a way for the caller to distinguish "we rejected this
// event" from "we accepted it". Both answer the webhook with 200, because
// Razorpay retries anything else, and retrying an event we have permanently
// refused would just produce the same refusal every few minutes for a day.
type outcome struct {
	// processError is nil when the event was applied. When it is set, the row in
	// payment_webhooks carries the reason and a human has something to read.
	processError *string
	// transient is set when the failure was infrastructure rather than the
	// event: the database was unreachable, a statement failed. It rolls the
	// transaction back and is returned to the caller as a 500, because this is
	// the one case where we want Razorpay to deliver the event again.
	transient error
}

// paidOrder is what the webhook needs to know about an order to settle it.
type paidOrder struct {
	ID         uuid.UUID
	Status     string
	TotalMinor int64
	Currency   string
	BuyerEmail string
}

var (
	// ErrInvalidSignature means the request did not come from Razorpay — or came
	// from Razorpay while we hold the wrong secret. The two are indistinguishable
	// from here, which is why the response says nothing about which.
	ErrInvalidSignature = errors.New("webhook signature is not valid")
	ErrMalformedEvent   = errors.New("webhook body is not a valid event")
)
