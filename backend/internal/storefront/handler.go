package storefront

import (
	"errors"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/mudit/creatorflow/backend/internal/analytics"
	"github.com/mudit/creatorflow/backend/internal/httpx"
)

// Recorder is declared here, where it is consumed: the storefront needs to
// record that something was viewed and to derive a visitor pseudonym, and
// nothing more. Keeping it an interface is also what lets these handlers be
// exercised with recording switched off.
//
// Nothing on this interface returns an error, which is the contract: analytics
// failing must never turn a working page into a broken one.
type Recorder interface {
	Record(ev analytics.Event)
	VisitorHash(ip, userAgent string, day time.Time) string
}

type Handler struct {
	svc      *Service
	recorder Recorder
}

func NewHandler(svc *Service, recorder Recorder) *Handler {
	return &Handler{svc: svc, recorder: recorder}
}

// visitor derives today's pseudonym for the caller.
//
// c.IP() is the peer address unless Fiber is configured to trust a proxy header,
// which matters behind a load balancer: get that wrong and every visitor shares
// the balancer's address and the unique count collapses to one. Worth revisiting
// in phase 15, when there actually is a load balancer.
func (h *Handler) visitor(c *fiber.Ctx) string {
	return h.recorder.VisitorHash(c.IP(), c.Get("User-Agent"), time.Now().UTC())
}

type creatorResponse struct {
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	Bio         *string `json:"bio"`
}

type linkResponse struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	// The path a client should actually link to, so the click is counted.
	ClickURL string `json:"click_url"`
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

func (h *Handler) Page(c *fiber.Ctx) error {
	page, err := h.svc.Page(c.Context(), c.Params("username"))
	if err != nil {
		return mapError(err)
	}

	links := make([]linkResponse, 0, len(page.Links))
	for _, l := range page.Links {
		links = append(links, linkResponse{
			Title: l.Title,
			URL:   l.URL,
			// Two URLs on purpose. URL is the real destination, so a visitor can
			// see where a link goes before clicking and a crawler can follow it;
			// ClickURL is our redirect, which records the click and then sends
			// them on. A page that only exposed the redirect would hide the
			// destination from the person deciding whether to click.
			ClickURL: "/api/v1/public/" + page.Creator.Username + "/links/" + l.ID.String() + "/go",
		})
	}

	products := make([]productSummaryResponse, 0, len(page.Products))
	for _, p := range page.Products {
		products = append(products, productSummaryResponse{
			Slug: p.Slug, Title: p.Title, PriceMinor: p.PriceMinor, Currency: p.Currency,
		})
	}

	// Recorded after the page was successfully assembled, so a 404 is not
	// counted as a view. Record() returns immediately and never fails.
	h.recorder.Record(analytics.Event{
		ProfileID:    page.ProfileID,
		Type:         analytics.EventProfileView,
		VisitorHash:  h.visitor(c),
		ReferrerHost: analytics.ReferrerHost(c.Get("Referer")),
	})

	return c.JSON(pageResponse{
		Creator:  newCreatorResponse(page.Creator),
		Links:    links,
		Products: products,
	})
}

func (h *Handler) Product(c *fiber.Ctx) error {
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

	productID := detail.ProductID
	h.recorder.Record(analytics.Event{
		ProfileID:    detail.ProfileID,
		Type:         analytics.EventProductView,
		VisitorHash:  h.visitor(c),
		ProductID:    &productID,
		ReferrerHost: analytics.ReferrerHost(c.Get("Referer")),
	})

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

// LinkClick records a click and forwards the visitor to the destination.
//
// A redirect rather than a client-side beacon. A beacon needs JavaScript, is
// blocked by every content blocker, and would mean another public write endpoint
// for anyone to spam. A redirect is counted server-side, works with JS disabled,
// and cannot be suppressed by the visitor's browser.
//
// What it costs, plainly: one extra hop before the destination loads, and the
// destination sees our domain in its Referer rather than the original page.
func (h *Handler) LinkClick(c *fiber.Ctx) error {
	linkID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_link_id", "That link id is not a valid identifier.")
	}

	username := c.Params("username")
	profileID, target, err := h.svc.LinkTarget(c.Context(), username, linkID)
	if err != nil {
		return mapError(err)
	}

	h.recorder.Record(analytics.Event{
		ProfileID:    profileID,
		Type:         analytics.EventLinkClick,
		VisitorHash:  h.visitor(c),
		LinkID:       &linkID,
		ReferrerHost: analytics.ReferrerHost(c.Get("Referer")),
	})

	// 302, not 301. A permanent redirect is cached by the browser forever, so
	// every later click would skip this endpoint entirely and go uncounted — and
	// the creator could never change where the link points.
	return c.Redirect(target, http.StatusFound)
}
