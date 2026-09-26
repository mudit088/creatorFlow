package analytics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Overview is a creator's dashboard for one date range.
//
// Everything here is aggregated at read time. The alternative — a rollup table
// maintained by a job — is faster on a large events table but adds a second
// source of truth that can drift from the first, needs backfilling whenever the
// definition of a metric changes, and is wrong in a way nobody notices for
// weeks. On an indexed range scan this is milliseconds, and the honest trigger
// for switching is a dashboard query that has become slow, not a row count
// someone guessed in advance.
type Overview struct {
	From time.Time
	To   time.Time

	ProfileViews int64
	ProductViews int64
	LinkClicks   int64
	Purchases    int64

	// DailyVisitorTotal is the sum of daily unique visitors, not unique people
	// across the range. Those are different numbers and only the first one is
	// knowable here: visitor hashes rotate at midnight precisely so that the
	// same person cannot be followed from one day to the next. Someone visiting
	// on three days counts three times, and that is the design working.
	DailyVisitorTotal int64

	// Revenue comes from orders, never from events. Events are a lossy,
	// best-effort record; money is not. If the two ever disagree, the orders
	// table is right.
	PaidOrders   int64
	RevenueMinor int64

	Days        []DayCount
	TopProducts []ProductStat
	TopLinks    []LinkStat
}

// DayCount is one row of the chart. Days with no activity are present with
// zeros, because a sparse series makes the client responsible for filling gaps
// and every client fills them differently.
type DayCount struct {
	Day          time.Time
	ProfileViews int64
	ProductViews int64
	LinkClicks   int64
	Purchases    int64
	Visitors     int64
}

type ProductStat struct {
	ProductID uuid.UUID
	Slug      string
	Title     string
	Views     int64
	Purchases int64
}

type LinkStat struct {
	LinkID uuid.UUID
	Title  string
	URL    string
	Clicks int64
}

var (
	// ErrNoProfile means the caller has not created their page yet, so there is
	// nothing to report on.
	ErrNoProfile = errors.New("create a profile before viewing analytics")
	ErrBadRange  = errors.New("invalid date range")
)

// maxRangeDays bounds how much a single dashboard request can scan. Without it,
// from=1970-01-01 is a full table scan that any authenticated user can ask for
// repeatedly.
const maxRangeDays = 366

// ResolveProfile turns the caller into the profile whose analytics they may see.
//
// Done once, rather than repeating "WHERE profile_id = (SELECT id FROM profiles
// WHERE user_id = $1)" in six queries. One place decides whose data this is,
// which is also one place to get wrong instead of six.
func (r *Repository) ResolveProfile(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	const q = `SELECT id FROM profiles WHERE user_id = $1`

	var id uuid.UUID
	err := r.db.QueryRow(ctx, q, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNoProfile
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve profile: %w", err)
	}
	return id, nil
}

// EventTotals counts each event type over the range.
//
// FILTER rather than four separate queries or a CASE sum: one pass over the
// index, and the intent reads directly. occurred_at is compared half-open
// (>= from, < to) so a day never belongs to two ranges.
func (r *Repository) EventTotals(ctx context.Context, profileID uuid.UUID, from, to time.Time) (profileViews, productViews, linkClicks, purchases int64, err error) {
	const q = `
		SELECT
		    count(*) FILTER (WHERE event_type = 'profile_view'),
		    count(*) FILTER (WHERE event_type = 'product_view'),
		    count(*) FILTER (WHERE event_type = 'link_click'),
		    count(*) FILTER (WHERE event_type = 'purchase')
		FROM events
		WHERE profile_id = $1 AND occurred_at >= $2 AND occurred_at < $3`

	err = r.db.QueryRow(ctx, q, profileID, from, to).Scan(&profileViews, &productViews, &linkClicks, &purchases)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("event totals: %w", err)
	}
	return profileViews, productViews, linkClicks, purchases, nil
}

// VisitorDays counts visitor rows, which is visitor-days rather than people.
func (r *Repository) VisitorDays(ctx context.Context, profileID uuid.UUID, from, to time.Time) (int64, error) {
	const q = `
		SELECT count(*) FROM visitors
		WHERE profile_id = $1 AND seen_on >= $2::date AND seen_on < $3::date`

	var n int64
	if err := r.db.QueryRow(ctx, q, profileID, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("visitor days: %w", err)
	}
	return n, nil
}

// Revenue reads the orders table, not events.
//
// paid_at is the date used, not created_at: an order created on the 30th and
// paid on the 1st is revenue for the new month, which is what an accountant and
// a creator both expect.
func (r *Repository) Revenue(ctx context.Context, profileID uuid.UUID, from, to time.Time) (orders, revenueMinor int64, err error) {
	const q = `
		SELECT count(*), coalesce(sum(total_minor), 0)
		FROM orders
		WHERE profile_id = $1 AND status = 'paid' AND paid_at >= $2 AND paid_at < $3`

	if err := r.db.QueryRow(ctx, q, profileID, from, to).Scan(&orders, &revenueMinor); err != nil {
		return 0, 0, fmt.Errorf("revenue: %w", err)
	}
	return orders, revenueMinor, nil
}

// DailySeries returns one row per day in the range, zeros included.
//
// generate_series produces the calendar and the events LEFT JOIN onto it, so a
// quiet Tuesday is a row of zeros rather than a gap. Doing this in SQL rather
// than in Go means the client, the service and any future CSV export all agree
// on what "no activity" looks like.
//
// Dates are grouped in UTC explicitly. Without the cast the grouping would
// follow the server's timezone, and a dashboard that silently changes shape
// when a server moves region is a bug nobody can reproduce.
func (r *Repository) DailySeries(ctx context.Context, profileID uuid.UUID, from, to time.Time) ([]DayCount, error) {
	const q = `
		WITH days AS (
		    SELECT generate_series($2::date, $3::date - 1, interval '1 day')::date AS day
		),
		ev AS (
		    SELECT (occurred_at AT TIME ZONE 'UTC')::date AS day,
		           count(*) FILTER (WHERE event_type = 'profile_view') AS profile_views,
		           count(*) FILTER (WHERE event_type = 'product_view') AS product_views,
		           count(*) FILTER (WHERE event_type = 'link_click')   AS link_clicks,
		           count(*) FILTER (WHERE event_type = 'purchase')     AS purchases
		    FROM events
		    WHERE profile_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		    GROUP BY 1
		),
		vis AS (
		    SELECT seen_on AS day, count(*) AS visitors
		    FROM visitors
		    WHERE profile_id = $1 AND seen_on >= $2::date AND seen_on < $3::date
		    GROUP BY 1
		)
		SELECT d.day,
		       coalesce(ev.profile_views, 0), coalesce(ev.product_views, 0),
		       coalesce(ev.link_clicks, 0), coalesce(ev.purchases, 0),
		       coalesce(vis.visitors, 0)
		FROM days d
		LEFT JOIN ev  ON ev.day  = d.day
		LEFT JOIN vis ON vis.day = d.day
		ORDER BY d.day`

	rows, err := r.db.Query(ctx, q, profileID, from, to)
	if err != nil {
		return nil, fmt.Errorf("daily series: %w", err)
	}
	defer rows.Close()

	out := make([]DayCount, 0)
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Day, &d.ProfileViews, &d.ProductViews, &d.LinkClicks, &d.Purchases, &d.Visitors); err != nil {
			return nil, fmt.Errorf("scan daily series: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TopProducts ranks by purchases first, then views. A product nobody views is
// absent rather than listed with zeros: the question is "what is working", and
// padding the answer with every draft makes it harder to read.
func (r *Repository) TopProducts(ctx context.Context, profileID uuid.UUID, from, to time.Time, limit int) ([]ProductStat, error) {
	const q = `
		SELECT p.id, p.slug, p.title,
		       count(*) FILTER (WHERE e.event_type = 'product_view') AS views,
		       count(*) FILTER (WHERE e.event_type = 'purchase')     AS purchases
		FROM events e
		JOIN products p ON p.id = e.product_id
		WHERE e.profile_id = $1 AND e.occurred_at >= $2 AND e.occurred_at < $3
		  AND e.product_id IS NOT NULL
		GROUP BY p.id, p.slug, p.title
		ORDER BY purchases DESC, views DESC, p.title
		LIMIT $4`

	rows, err := r.db.Query(ctx, q, profileID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("top products: %w", err)
	}
	defer rows.Close()

	out := make([]ProductStat, 0)
	for rows.Next() {
		var p ProductStat
		if err := rows.Scan(&p.ProductID, &p.Slug, &p.Title, &p.Views, &p.Purchases); err != nil {
			return nil, fmt.Errorf("scan product stat: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) TopLinks(ctx context.Context, profileID uuid.UUID, from, to time.Time, limit int) ([]LinkStat, error) {
	const q = `
		SELECT l.id, l.title, l.url, count(*) AS clicks
		FROM events e
		JOIN links l ON l.id = e.link_id
		WHERE e.profile_id = $1 AND e.occurred_at >= $2 AND e.occurred_at < $3
		  AND e.event_type = 'link_click'
		GROUP BY l.id, l.title, l.url
		ORDER BY clicks DESC, l.title
		LIMIT $4`

	rows, err := r.db.Query(ctx, q, profileID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("top links: %w", err)
	}
	defer rows.Close()

	out := make([]LinkStat, 0)
	for rows.Next() {
		var l LinkStat
		if err := rows.Scan(&l.LinkID, &l.Title, &l.URL, &l.Clicks); err != nil {
			return nil, fmt.Errorf("scan link stat: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Overview assembles the dashboard.
//
// Five queries rather than one statement with five CTEs. They hit three
// different indexes and none depends on another's result, so the single-query
// version would be harder to read, harder to change, and no faster in a way
// anyone could measure at this size. If it ever matters, they can be merged —
// and by then there will be a number saying it was worth doing.
func (s *Service) Overview(ctx context.Context, userID uuid.UUID, from, to time.Time) (*Overview, error) {
	if !to.After(from) {
		return nil, ErrBadRange
	}
	if to.Sub(from) > maxRangeDays*24*time.Hour {
		return nil, ErrBadRange
	}

	profileID, err := s.repo.ResolveProfile(ctx, userID)
	if err != nil {
		return nil, err
	}

	o := &Overview{From: from, To: to}

	if o.ProfileViews, o.ProductViews, o.LinkClicks, o.Purchases, err =
		s.repo.EventTotals(ctx, profileID, from, to); err != nil {
		return nil, err
	}
	if o.DailyVisitorTotal, err = s.repo.VisitorDays(ctx, profileID, from, to); err != nil {
		return nil, err
	}
	if o.PaidOrders, o.RevenueMinor, err = s.repo.Revenue(ctx, profileID, from, to); err != nil {
		return nil, err
	}
	if o.Days, err = s.repo.DailySeries(ctx, profileID, from, to); err != nil {
		return nil, err
	}
	if o.TopProducts, err = s.repo.TopProducts(ctx, profileID, from, to, 5); err != nil {
		return nil, err
	}
	if o.TopLinks, err = s.repo.TopLinks(ctx, profileID, from, to, 5); err != nil {
		return nil, err
	}

	return o, nil
}
