package storefront

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

// RegisterRoutes mounts the only endpoints in this API that take no
// authentication at all. Everything here is world-readable by design, which is
// exactly why the types it returns are separate from the owner-facing ones.
func (h *Handler) RegisterRoutes(r fiber.Router) {
	g := r.Group("/public")
	g.Get("/:username", h.page)
	g.Get("/:username/:slug", h.product)
}

type creatorResponse struct {
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	Bio         *string `json:"bio"`
}

type linkResponse struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type productSummaryResponse struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	PriceMinor int64  `json:"price_minor"`
	Currency   string `json:"currency"`
}

type pageResponse struct {
	Creator  creatorResponse          `json:"creator"`
	Links    []linkResponse           `json:"links"`
	Products []productSummaryResponse `json:"products"`
}

type deliverableResponse struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type productDetailResponse struct {
	Creator     creatorResponse       `json:"creator"`
	Slug        string                `json:"slug"`
	Title       string                `json:"title"`
	Description *string               `json:"description"`
	PriceMinor  int64                 `json:"price_minor"`
	Currency    string                `json:"currency"`
	Includes    []deliverableResponse `json:"includes"`
}

func (h *Handler) page(c *fiber.Ctx) error {
	page, err := h.svc.Page(c.Context(), c.Params("username"))
	if err != nil {
		return mapError(err)
	}

	links := make([]linkResponse, 0, len(page.Links))
	for _, l := range page.Links {
		links = append(links, linkResponse{Title: l.Title, URL: l.URL})
	}

	products := make([]productSummaryResponse, 0, len(page.Products))
	for _, p := range page.Products {
		products = append(products, productSummaryResponse{
			Slug: p.Slug, Title: p.Title, PriceMinor: p.PriceMinor, Currency: p.Currency,
		})
	}

	return c.JSON(pageResponse{
		Creator:  newCreatorResponse(page.Creator),
		Links:    links,
		Products: products,
	})
}

func (h *Handler) product(c *fiber.Ctx) error {
	detail, err := h.svc.Product(c.Context(), c.Params("username"), c.Params("slug"))
	if err != nil {
		return mapError(err)
	}

	includes := make([]deliverableResponse, 0, len(detail.Includes))
	for _, d := range detail.Includes {
		includes = append(includes, deliverableResponse{
			Name: d.Name, ContentType: d.ContentType, SizeBytes: d.SizeBytes,
		})
	}

	return c.JSON(productDetailResponse{
		Creator:     newCreatorResponse(detail.Creator),
		Slug:        detail.Slug,
		Title:       detail.Title,
		Description: detail.Description,
		PriceMinor:  detail.PriceMinor,
		Currency:    detail.Currency,
		Includes:    includes,
	})
}

func mapError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return httpx.New(http.StatusNotFound, "not_found", "No such page.")
	}
	return httpx.ErrInternal.WithCause(err)
}

func newCreatorResponse(c Creator) creatorResponse {
	return creatorResponse{Username: c.Username, DisplayName: c.DisplayName, Bio: c.Bio}
}
