package delivery

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/mudit/creatorflow/backend/internal/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// downloadRequest carries the second factor. The order id in the path is an
// unguessable uuid; the email proves the holder is the buyer rather than anyone
// who saw the id in a browser history, a shared screen or a support ticket.
type downloadRequest struct {
	BuyerEmail string `json:"buyer_email"`
}

type downloadResponse struct {
	OrderID string         `json:"order_id"`
	Files   []linkResponse `json:"files"`
}

// linkResponse omits the S3 key, as the product endpoints do. The signed URL is
// the only address a client needs, and publishing the bucket layout only helps
// someone probing for objects.
type linkResponse struct {
	ProductID   string `json:"product_id"`
	Product     string `json:"product"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	URL         string `json:"url"`
	ExpiresIn   int    `json:"expires_in"`
}

// Download is public, like checkout, because buyers are guests.
func (h *Handler) Download(c *fiber.Ctx) error {
	orderID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		// A malformed id is a client bug rather than a failed guess, so it gets a
		// distinct answer. It reveals nothing: it is true of every string that is
		// not a uuid, whether or not an order exists.
		return httpx.New(http.StatusBadRequest, "invalid_order_id", "That order id is not a valid identifier.")
	}

	var req downloadRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	links, err := h.svc.Download(c.Context(), orderID, req.BuyerEmail)
	if errors.Is(err, ErrNotEntitled) {
		// One answer for every refusal — unknown order, wrong email, revoked
		// access, unpaid order. Anything more specific turns this endpoint into a
		// way to confirm who bought what.
		return httpx.New(http.StatusNotFound, "not_entitled",
			"No downloads are available for that order and email address.")
	}
	if err != nil {
		return httpx.ErrInternal.WithCause(err)
	}

	out := downloadResponse{OrderID: orderID.String(), Files: make([]linkResponse, 0, len(links))}
	for _, l := range links {
		out.Files = append(out.Files, linkResponse{
			ProductID:   l.ProductID.String(),
			Product:     l.ProductTitle,
			Filename:    l.Filename,
			ContentType: l.ContentType,
			SizeBytes:   l.SizeBytes,
			URL:         l.URL,
			ExpiresIn:   int(l.ExpiresIn.Seconds()),
		})
	}
	return c.JSON(out)
}
