package payments

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gofiber/fiber/v2"

	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Webhook receives Razorpay's event deliveries.
//
// This handler deliberately does not use c.BodyParser. The signature covers the
// exact bytes Razorpay sent, and unmarshalling then re-marshalling reorders keys
// and changes whitespace, so the hash of the round-tripped body is not the hash
// that was signed. c.Body() and nothing else.
func (h *Handler) Webhook(c *fiber.Ctx) error {
	raw := c.Body()
	signature := c.Get("X-Razorpay-Signature")

	// Razorpay sends a unique id per event, which is what makes deduplication
	// possible across its retries. If it is ever absent, the body's own hash is
	// a safe substitute: identical bytes are the same delivery, and any real
	// difference produces a different id.
	eventID := c.Get("X-Razorpay-Event-Id")
	if eventID == "" {
		sum := sha256.Sum256(raw)
		eventID = "body-" + hex.EncodeToString(sum[:])
	}

	err := h.svc.Handle(c.Context(), raw, signature, eventID)

	switch {
	case err == nil:
		return c.SendStatus(http.StatusOK)

	case errors.Is(err, ErrInvalidSignature):
		// Logged rather than stored. An unauthenticated endpoint that writes a
		// row per request is a table anyone on the internet can fill, so until
		// the rate limiter lands in phase 11, rejected deliveries live in the
		// log — where a burst of them is still visible as either a wrong secret
		// or someone probing.
		slog.Warn("rejected webhook with invalid signature",
			"request_id", middleware.FromContext(c), "ip", c.IP(), "bytes", len(raw))
		// 401 with no detail. Telling a caller whether the secret or the body
		// was wrong helps only the caller who is guessing.
		return httpx.New(http.StatusUnauthorized, "invalid_signature", "Signature verification failed.")

	case errors.Is(err, ErrMalformedEvent):
		// The signature was valid, so this really did come from Razorpay — a
		// shape we do not understand rather than an attack. 400 tells them not
		// to retry it.
		slog.Warn("valid signature but unparseable event body", "request_id", middleware.FromContext(c))
		return httpx.New(http.StatusBadRequest, "malformed_event", "Event body could not be parsed.")

	default:
		// Anything else is our problem: the database was unreachable, a
		// statement failed. 500 is the correct answer because it is the one that
		// makes Razorpay deliver the event again, and the transaction rolled
		// back, so the retry will find nothing recorded and redo the work.
		return httpx.ErrInternal.WithCause(err)
	}
}
