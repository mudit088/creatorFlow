package analytics

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dashboard builds a profile with a product, a link, events spread over known
// days, and one paid order — enough to check that every number lands in the
// right bucket.
type dashboard struct {
	userID    uuid.UUID
	profileID uuid.UUID
	productID uuid.UUID
	linkID    uuid.UUID
}

func newDashboard(t *testing.T, pool *pgxpool.Pool) dashboard {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var d dashboard

	must := func(step string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("user", pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("rep-test-%s@test.local", suffix)).Scan(&d.userID))
	must("profile", pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'R') RETURNING id`,
		d.userID, "reptest"+suffix).Scan(&d.profileID))
	must("product", pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor, status, published_at)
		 VALUES ($1, $2, 'Plan', 49900, 'published', now()) RETURNING id`,
		d.profileID, "p-"+suffix).Scan(&d.productID))
	must("link", pool.QueryRow(ctx,
		`INSERT INTO links (profile_id, title, url, position) VALUES ($1, 'YT', 'https://y.test', 1) RETURNING id`,
		d.profileID).Scan(&d.linkID))

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM events WHERE profile_id = $1`, d.profileID)
		_, _ = pool.Exec(c, `DELETE FROM visitors WHERE profile_id = $1`, d.profileID)
		_, _ = pool.Exec(c, `DELETE FROM orders WHERE profile_id = $1`, d.profileID)
		_, _ = pool.Exec(c, `DELETE FROM links WHERE profile_id = $1`, d.profileID)
		_, _ = pool.Exec(c, `DELETE FROM products WHERE id = $1`, d.productID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, d.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, d.userID)
	})
	return d
}

// event inserts directly with an explicit timestamp, because the whole point is
// to control which day a row lands on.
func (d dashboard) event(t *testing.T, pool *pgxpool.Pool, eventType string, at time.Time, productID, linkID, orderID *uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO events (profile_id, event_type, product_id, link_id, order_id, occurred_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		d.profileID, eventType, productID, linkID, orderID, at); err != nil {
		t.Fatalf("insert %s event: %v", eventType, err)
	}
}

func (d dashboard) paidOrder(t *testing.T, pool *pgxpool.Pool, amount int64, paidAt time.Time) uuid.UUID {
	t.Helper()
	var orderID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO orders (profile_id, buyer_email, total_minor, status, paid_at)
		 VALUES ($1, 'buyer@test.local', $2, 'paid', $3) RETURNING id`,
		d.profileID, amount, paidAt).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	return orderID
}

func TestOverviewCountsEachMetric(t *testing.T) {
	pool := testPool(t)
	d := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)

	d.event(t, pool, EventProfileView, now, nil, nil, nil)
	d.event(t, pool, EventProfileView, yesterday, nil, nil, nil)
	d.event(t, pool, EventProductView, now, &d.productID, nil, nil)
	d.event(t, pool, EventLinkClick, now, nil, &d.linkID, nil)

	orderID := d.paidOrder(t, pool, 49900, now)
	d.event(t, pool, EventPurchase, now, &d.productID, nil, &orderID)

	from := now.AddDate(0, 0, -7)
	to := now.AddDate(0, 0, 1)

	o, err := svc.Overview(context.Background(), d.userID, from, to)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}

	if o.ProfileViews != 2 || o.ProductViews != 1 || o.LinkClicks != 1 || o.Purchases != 1 {
		t.Errorf("totals = views %d/%d clicks %d purchases %d, want 2/1/1/1",
			o.ProfileViews, o.ProductViews, o.LinkClicks, o.Purchases)
	}
	// Revenue comes from orders, not from the purchase event.
	if o.PaidOrders != 1 || o.RevenueMinor != 49900 {
		t.Errorf("revenue = %d orders / %d minor, want 1 / 49900", o.PaidOrders, o.RevenueMinor)
	}
	if len(o.TopProducts) != 1 || o.TopProducts[0].Purchases != 1 || o.TopProducts[0].Views != 1 {
		t.Errorf("top products = %+v, want one product with 1 view and 1 purchase", o.TopProducts)
	}
	if len(o.TopLinks) != 1 || o.TopLinks[0].Clicks != 1 {
		t.Errorf("top links = %+v, want one link with 1 click", o.TopLinks)
	}
}

// The range is half-open: `to` is exclusive, so a day never belongs to two
// adjacent ranges and "September" plus "October" counts everything once.
func TestRangeIsHalfOpen(t *testing.T) {
	pool := testPool(t)
	d := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	boundary := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	d.event(t, pool, EventProfileView, boundary, nil, nil, nil)

	ctx := context.Background()

	// A range ending at the boundary day excludes it.
	before, err := svc.Overview(ctx, d.userID,
		time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if before.ProfileViews != 0 {
		t.Errorf("views before boundary = %d, want 0 — `to` must be exclusive", before.ProfileViews)
	}

	// A range starting on it includes it.
	after, err := svc.Overview(ctx, d.userID,
		time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if after.ProfileViews != 1 {
		t.Errorf("views from boundary = %d, want 1 — `from` must be inclusive", after.ProfileViews)
	}
}

// A quiet day is a row of zeros, not a gap. Otherwise every client invents its
// own way to fill the hole and the charts disagree.
func TestDailySeriesIncludesEmptyDays(t *testing.T) {
	pool := testPool(t)
	d := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	d.event(t, pool, EventProfileView, from.AddDate(0, 0, 2).Add(9*time.Hour), nil, nil, nil)

	o, err := svc.Overview(context.Background(), d.userID, from, to)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}

	if len(o.Days) != 7 {
		t.Fatalf("days = %d, want 7 (one row per day in the range)", len(o.Days))
	}
	if o.Days[2].ProfileViews != 1 {
		t.Errorf("day 3 views = %d, want 1", o.Days[2].ProfileViews)
	}
	if o.Days[0].ProfileViews != 0 || o.Days[6].ProfileViews != 0 {
		t.Error("quiet days should be present with zeros")
	}
}

// One creator must never see another's numbers. The scoping is in the SQL, so
// this is really a test that the WHERE clause exists at all.
func TestOverviewIsScopedToTheCaller(t *testing.T) {
	pool := testPool(t)
	mine := newDashboard(t, pool)
	theirs := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		theirs.event(t, pool, EventProfileView, now, nil, nil, nil)
	}
	orderID := theirs.paidOrder(t, pool, 99900, now)
	theirs.event(t, pool, EventPurchase, now, &theirs.productID, nil, &orderID)

	o, err := svc.Overview(context.Background(), mine.userID, now.AddDate(0, 0, -7), now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if o.ProfileViews != 0 || o.RevenueMinor != 0 {
		t.Errorf("saw another creator's data: views %d, revenue %d", o.ProfileViews, o.RevenueMinor)
	}
}

// Revenue is attributed by paid_at, not created_at: an order created on the
// 30th and paid on the 1st is revenue for the new month.
func TestRevenueUsesPaidAtNotCreatedAt(t *testing.T) {
	pool := testPool(t)
	d := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	ctx := context.Background()
	paidAt := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	created := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)

	var orderID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orders (profile_id, buyer_email, total_minor, status, paid_at, created_at)
		 VALUES ($1, 'buyer@test.local', 49900, 'paid', $2, $3) RETURNING id`,
		d.profileID, paidAt, created).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}

	september, err := svc.Overview(ctx, d.userID,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if september.RevenueMinor != 0 {
		t.Errorf("september revenue = %d, want 0 — it was paid in October", september.RevenueMinor)
	}

	october, err := svc.Overview(ctx, d.userID,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if october.RevenueMinor != 49900 {
		t.Errorf("october revenue = %d, want 49900", october.RevenueMinor)
	}
}

// An unpaid order is not revenue, however much someone intended to spend.
func TestPendingOrdersAreNotRevenue(t *testing.T) {
	pool := testPool(t)
	d := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO orders (profile_id, buyer_email, total_minor, status)
		 VALUES ($1, 'buyer@test.local', 49900, 'pending')`, d.profileID); err != nil {
		t.Fatalf("insert pending order: %v", err)
	}

	now := time.Now().UTC()
	o, err := svc.Overview(context.Background(), d.userID, now.AddDate(0, 0, -7), now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if o.RevenueMinor != 0 || o.PaidOrders != 0 {
		t.Errorf("pending order counted as revenue: %d minor across %d orders", o.RevenueMinor, o.PaidOrders)
	}
}

// A range large enough to scan the whole table is refused, so one authenticated
// caller cannot turn the dashboard into a repeatable full-table scan.
func TestAbsurdRangeIsRefused(t *testing.T) {
	pool := testPool(t)
	d := newDashboard(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")
	t.Cleanup(svc.Close)

	_, err := svc.Overview(context.Background(), d.userID,
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC())
	if !errors.Is(err, ErrBadRange) {
		t.Fatalf("error = %v, want ErrBadRange", err)
	}

	if _, err := svc.Overview(context.Background(), d.userID,
		time.Now().UTC(), time.Now().UTC().AddDate(0, 0, -1)); !errors.Is(err, ErrBadRange) {
		t.Fatalf("backwards range error = %v, want ErrBadRange", err)
	}
}

func TestConversionRate(t *testing.T) {
	cases := []struct {
		purchases, views int64
		want             float64
	}{
		{1, 4, 25},
		{1, 3, 33.3},
		{0, 0, 0}, // no views: not NaN, which JSON cannot encode
		{2, 0, 0},
		{3, 3, 100},
	}
	for _, tc := range cases {
		if got := conversionRate(tc.purchases, tc.views); got != tc.want {
			t.Errorf("conversionRate(%d, %d) = %v, want %v", tc.purchases, tc.views, got, tc.want)
		}
	}
}
