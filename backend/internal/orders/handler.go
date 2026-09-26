package orders

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"

	"github.com/mudit/creatorflow/backend/internal/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// createOrderRequest carries no amount, and it never will. The total is the
// database's answer, not the client's claim.
type createOrderRequest struct {
	BuyerEmail string   `json:"buyer_email"`
	ProductIDs []string `json:"product_ids"`
}

type orderResponse struct {
	ID         string              `json:"id"`
	Status     string              `json:"status"`
	BuyerEmail string              `json:"buyer_email"`
	TotalMinor int64               `json:"total_minor"`
	Currency   string              `json:"currency"`
	Items      []orderItemResponse `json:"items"`
	CreatedAt  string              `json:"created_at"`
	// Absent when Razorpay is not configured, which is the normal state in
	// local development. A client seeing no checkout block should say "payments
	// are unavailable", not open an empty payment window.
	Checkout *checkoutResponse `json:"checkout,omitempty"`
}

// checkoutResponse is exactly what Razorpay's browser script needs, and nothing
// else. KeyID is the public half of the key pair — the secret never leaves the
// server. AmountMinor is repeated here rather than read from total_minor by the
// client so that the number handed to the payment window and the number we
// registered with Razorpay are visibly the same value.
type checkoutResponse struct {
	Provider        string `json:"provider"`
	KeyID           string `json:"key_id"`
	ProviderOrderID string `json:"provider_order_id"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
}

// orderItemResponse returns the snapshot, not the product's current state. A
// client rendering "you are buying X at Y" must show what this order actually
// froze, or the confirmation screen and the receipt can disagree.
type orderItemResponse struct {
	ProductID      string `json:"product_id"`
	Title          string `json:"title"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Quantity       int    `json:"quantity"`
}

func (h *Handler) Create(c *fiber.Ctx) error {
	var req createOrderRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	// The header, not a body field. Idempotency is a property of the request,
	// and keeping it out of the payload means a client can retry a byte-identical
	// body and have it mean "the same attempt" rather than "a second one".
	var idempotencyKey *string
	if key := c.Get("Idempotency-Key"); key != "" {
		idempotencyKey = &key
	}

	order, items, replayed, err := h.svc.Create(c.Context(), req.BuyerEmail, req.ProductIDs, idempotencyKey)
	if err != nil {
		return mapError(err)
	}

	// 200 on a replay, 201 on a create. A client that retried gets a success
	// either way, and the status code tells it which of the two happened without
	// having to compare ids.
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	return c.Status(status).JSON(newOrderResponse(order, items, h.svc.PublicKeyID()))
}

func newOrderResponse(o *Order, items []Item, keyID string) orderResponse {
	out := orderResponse{
		ID:         o.ID.String(),
		Status:     o.Status,
		BuyerEmail: o.BuyerEmail,
		TotalMinor: o.TotalMinor,
		Currency:   o.Currency,
		Items:      make([]orderItemResponse, 0, len(items)),
		CreatedAt:  o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	for _, i := range items {
		out.Items = append(out.Items, orderItemResponse{
			ProductID:      i.ProductID.String(),
			Title:          i.TitleSnapshot,
			UnitPriceMinor: i.UnitPriceMinor,
			Quantity:       i.Quantity,
		})
	}

	// Both halves have to be present. A provider order id with no key, or a key
	// with no provider order, cannot open a payment window — so rather than
	// emitting half a block for the client to discover at runtime, emit none.
	if o.ProviderOrderID != nil && keyID != "" {
		out.Checkout = &checkoutResponse{
			Provider:        "razorpay",
			KeyID:           keyID,
			ProviderOrderID: *o.ProviderOrderID,
			AmountMinor:     o.TotalMinor,
			Currency:        o.Currency,
		}
	}
	return out
}

func mapError(err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return httpx.Validation(map[string]string{ve.Field: ve.Message})
	}

	switch {
	case errors.Is(err, ErrProductUnavailable):
		// 422 rather than 404, and deliberately vague. The caller is anonymous,
		// so distinguishing "no such product" from "that product is a draft"
		// would hand anyone a way to probe a creator's unreleased catalogue.
		return httpx.Validation(map[string]string{
			"product_ids": "One or more of these products is no longer available.",
		})
	case errors.Is(err, ErrMixedCreators):
		return httpx.New(http.StatusUnprocessableEntity, "mixed_creators", "An order can only contain products from one creator.")
	case errors.Is(err, ErrMixedCurrency):
		return httpx.New(http.StatusUnprocessableEntity, "mixed_currency", "An order can only contain products in one currency.")
	case errors.Is(err, ErrFreeCheckout):
		return httpx.New(http.StatusUnprocessableEntity, "free_checkout", "Free products do not go through checkout.")
	case errors.Is(err, ErrPaymentProviderUnavailable):
		// 503 with the cause logged. The order exists and is pending, so the
		// honest instruction to the client is "retry this same request" — with
		// the same Idempotency-Key, which resumes rather than duplicates.
		return httpx.New(http.StatusServiceUnavailable, "payment_provider_unavailable",
			"Payments are temporarily unavailable. Retry this request shortly.").WithCause(err)
	case errors.Is(err, ErrOrderNotFound):
		return httpx.ErrNotFound
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}
