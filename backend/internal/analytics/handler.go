package analytics

import (
	"errors"
	"net/http"
	"time"

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

const dateLayout = "2006-01-02"

type overviewResponse struct {
	From string `json:"from"`
	To   string `json:"to"`

	Totals   totalsResponse        `json:"totals"`
	Revenue  revenueResponse       `json:"revenue"`
	Days     []dayResponse         `json:"days"`
	Products []productStatResponse `json:"top_products"`
	Links    []linkStatResponse    `json:"top_links"`
}

type totalsResponse struct {
	ProfileViews int64 `json:"profile_views"`
	ProductViews int64 `json:"product_views"`
	LinkClicks   int64 `json:"link_clicks"`
	Purchases    int64 `json:"purchases"`
	// Named for what it actually is. It is the sum of each day's unique
	// visitors, not the number of distinct people over the range — visitor
	// hashes rotate at midnight so that the second number cannot be computed,
	// and a field called "unique_visitors" would be a quiet lie.
	DailyVisitorTotal int64 `json:"daily_visitor_total"`
	// Purchases per product view, as a percentage, rounded to one decimal.
	// Computed server-side so every client shows the same number.
	ConversionRate float64 `json:"conversion_rate_percent"`
}

type revenueResponse struct {
	PaidOrders int64 `json:"paid_orders"`
	// Minor units, like every other amount in this API. A client that wants
	// rupees divides by 100 at the point it renders, never before.
	RevenueMinor int64  `json:"revenue_minor"`
	Currency     string `json:"currency"`
}

type dayResponse struct {
	Day          string `json:"day"`
	ProfileViews int64  `json:"profile_views"`
	ProductViews int64  `json:"product_views"`
	LinkClicks   int64  `json:"link_clicks"`
	Purchases    int64  `json:"purchases"`
	Visitors     int64  `json:"visitors"`
}

type productStatResponse struct {
	ProductID string `json:"product_id"`
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Views     int64  `json:"views"`
	Purchases int64  `json:"purchases"`
}

type linkStatResponse struct {
	LinkID string `json:"link_id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Clicks int64  `json:"clicks"`
}

// Overview serves the creator dashboard.
//
// Authenticated, and scoped to the caller's own profile inside the queries —
// there is no profile id in the path or the query string, so there is nothing to
// tamper with. Asking for someone else's numbers is not forbidden here, it is
// unexpressible.
func (h *Handler) Overview(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	from, to, err := parseRange(c.Query("from"), c.Query("to"))
	if err != nil {
		return httpx.Validation(map[string]string{
			"from": "Use from=YYYY-MM-DD and to=YYYY-MM-DD, with to after from and at most 366 days apart.",
		})
	}

	overview, svcErr := h.svc.Overview(c.Context(), userID, from, to)
	switch {
	case errors.Is(svcErr, ErrNoProfile):
		return httpx.New(http.StatusConflict, "profile_required", "Create your profile before viewing analytics.")
	case errors.Is(svcErr, ErrBadRange):
		return httpx.Validation(map[string]string{
			"from": "Use a range with to after from and at most 366 days apart.",
		})
	case svcErr != nil:
		return httpx.ErrInternal.WithCause(svcErr)
	}

	return c.JSON(newOverviewResponse(overview))
}

// parseRange defaults to the last 30 days and treats `to` as exclusive.
//
// A half-open range is what makes "the last 30 days" and "September" both
// expressible without an off-by-one: to=2026-10-01 means everything before
// October, whatever the clock says at the instant of the request.
func parseRange(fromParam, toParam string) (time.Time, time.Time, error) {
	// Today in UTC, so a range means the same thing regardless of where the
	// server or the caller happens to be.
	today := time.Now().UTC().Truncate(24 * time.Hour)

	to := today.AddDate(0, 0, 1) // exclusive: includes everything so far today
	from := to.AddDate(0, 0, -30)

	var err error
	if fromParam != "" {
		if from, err = time.ParseInLocation(dateLayout, fromParam, time.UTC); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if toParam != "" {
		if to, err = time.ParseInLocation(dateLayout, toParam, time.UTC); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, ErrBadRange
	}
	return from, to, nil
}

func newOverviewResponse(o *Overview) overviewResponse {
	out := overviewResponse{
		From: o.From.Format(dateLayout),
		To:   o.To.Format(dateLayout),
		Totals: totalsResponse{
			ProfileViews:      o.ProfileViews,
			ProductViews:      o.ProductViews,
			LinkClicks:        o.LinkClicks,
			Purchases:         o.Purchases,
			DailyVisitorTotal: o.DailyVisitorTotal,
			ConversionRate:    conversionRate(o.Purchases, o.ProductViews),
		},
		Revenue: revenueResponse{
			PaidOrders:   o.PaidOrders,
			RevenueMinor: o.RevenueMinor,
			Currency:     "INR",
		},
		Days:     make([]dayResponse, 0, len(o.Days)),
		Products: make([]productStatResponse, 0, len(o.TopProducts)),
		Links:    make([]linkStatResponse, 0, len(o.TopLinks)),
	}

	for _, d := range o.Days {
		out.Days = append(out.Days, dayResponse{
			Day:          d.Day.Format(dateLayout),
			ProfileViews: d.ProfileViews,
			ProductViews: d.ProductViews,
			LinkClicks:   d.LinkClicks,
			Purchases:    d.Purchases,
			Visitors:     d.Visitors,
		})
	}
	for _, p := range o.TopProducts {
		out.Products = append(out.Products, productStatResponse{
			ProductID: p.ProductID.String(), Slug: p.Slug, Title: p.Title,
			Views: p.Views, Purchases: p.Purchases,
		})
	}
	for _, l := range o.TopLinks {
		out.Links = append(out.Links, linkStatResponse{
			LinkID: l.LinkID.String(), Title: l.Title, URL: l.URL, Clicks: l.Clicks,
		})
	}
	return out
}

// conversionRate guards the division rather than returning NaN. A product with
// no views has no conversion rate, and JSON has no way to encode NaN, so every
// client would break on it.
func conversionRate(purchases, views int64) float64 {
	if views <= 0 {
		return 0
	}
	rate := float64(purchases) / float64(views) * 100
	// One decimal place: these are directional numbers derived from a lossy
	// event stream, and four decimals would imply a precision that does not
	// exist.
	return float64(int64(rate*10+0.5)) / 10
}
