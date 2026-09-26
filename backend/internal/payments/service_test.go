package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/config"
	"github.com/mudit/creatorflow/backend/internal/razorpay"
)

// The webhook is the one place in this system where an HTTP request causes money
// to be treated as received, so these tests run against a real PostgreSQL and
// the real signature verifier. A mock of either would be testing the mock.
//
// Skipped without DATABASE_URL so the suite still passes with no infrastructure.

const webhookSecret = "test-webhook-secret"

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping database-backed test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("database unreachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testService(pool *pgxpool.Pool) *Service {
	// The real verifier, holding a known secret — so these tests exercise the
	// same HMAC path production uses.
	verifier := razorpay.New(&config.Config{RazorpayWebhookSecret: webhookSecret})
	return NewService(NewRepository(pool), verifier)
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// fixture is a pending order with a Razorpay order id attached — the exact state
// an order is in when a webhook arrives for it.
type fixture struct {
	orderID         uuid.UUID
	productID       uuid.UUID
	profileID       uuid.UUID
	userID          uuid.UUID
	providerOrderID string
	total           int64
	buyerEmail      string
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	f := fixture{
		providerOrderID: "order_test_" + suffix,
		total:           49900,
		buyerEmail:      fmt.Sprintf("buyer-%s@test.local", suffix),
	}

	must := func(step string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("user", pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("pay-test-%s@test.local", suffix)).Scan(&f.userID))

	must("profile", pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'Test') RETURNING id`,
		f.userID, "paytest"+suffix).Scan(&f.profileID))

	must("product", pool.QueryRow(ctx,
		`INSERT INTO products (profile_id, slug, title, price_minor, status, published_at)
		 VALUES ($1, $2, 'Test Product', $3, 'published', now()) RETURNING id`,
		f.profileID, "p-"+suffix, f.total).Scan(&f.productID))

	must("order", pool.QueryRow(ctx,
		`INSERT INTO orders (profile_id, buyer_email, total_minor, provider_order_id)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		f.profileID, f.buyerEmail, f.total, f.providerOrderID).Scan(&f.orderID))

	_, err := pool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, title_snapshot, unit_price_minor)
		 VALUES ($1, $2, 'Test Product', $3)`, f.orderID, f.productID, f.total)
	must("order item", err)

	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM entitlements WHERE order_id = $1`,
			`DELETE FROM payments WHERE order_id = $1`,
		} {
			_, _ = pool.Exec(c, q, f.orderID)
		}
		_, _ = pool.Exec(c, `DELETE FROM payment_webhooks WHERE raw_payload->'payload'->'payment'->'entity'->>'order_id' = $1`, f.providerOrderID)
		_, _ = pool.Exec(c, `DELETE FROM orders WHERE id = $1`, f.orderID)
		_, _ = pool.Exec(c, `DELETE FROM products WHERE id = $1`, f.productID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, f.profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, f.userID)
	})

	return f
}

func capturedEvent(providerOrderID, paymentID string, amount int64, currency string) []byte {
	return []byte(fmt.Sprintf(
		`{"entity":"event","event":"payment.captured","contains":["payment"],`+
			`"payload":{"payment":{"entity":{"id":%q,"order_id":%q,"amount":%d,"currency":%q,"status":"captured"}}}}`,
		paymentID, providerOrderID, amount, currency))
}

type orderState struct {
	status       string
	paidAt       *time.Time
	payments     int
	entitlements int
}

func readState(t *testing.T, pool *pgxpool.Pool, f fixture) orderState {
	t.Helper()
	ctx := context.Background()
	var s orderState

	if err := pool.QueryRow(ctx, `SELECT status, paid_at FROM orders WHERE id = $1`, f.orderID).
		Scan(&s.status, &s.paidAt); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE order_id = $1`, f.orderID).
		Scan(&s.payments); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE order_id = $1`, f.orderID).
		Scan(&s.entitlements); err != nil {
		t.Fatalf("count entitlements: %v", err)
	}
	return s
}

func webhookError(t *testing.T, pool *pgxpool.Pool, eventID string) (processed bool, processError *string) {
	t.Helper()
	var at *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT processed_at, process_error FROM payment_webhooks WHERE provider_event_id = $1`,
		eventID).Scan(&at, &processError)
	if err != nil {
		t.Fatalf("read webhook row: %v", err)
	}
	return at != nil, processError
}

// The happy path, and the invariant the whole phase exists for: paid and
// entitled are decided together, in one transaction.
func TestCapturedEventSettlesOrderAndGrantsEntitlement(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := testService(pool)

	body := capturedEvent(f.providerOrderID, "pay_test_1", f.total, "INR")
	eventID := "evt_" + uuid.NewString()

	if err := svc.Handle(context.Background(), body, sign(body), eventID); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	s := readState(t, pool, f)
	if s.status != "paid" || s.paidAt == nil {
		t.Errorf("order = %s (paid_at %v), want paid with a timestamp", s.status, s.paidAt)
	}
	if s.payments != 1 {
		t.Errorf("payments = %d, want 1", s.payments)
	}
	if s.entitlements != 1 {
		t.Errorf("entitlements = %d, want 1 — paid without access is the failure this table prevents", s.entitlements)
	}

	processed, processErr := webhookError(t, pool, eventID)
	if !processed || processErr != nil {
		t.Errorf("webhook processed=%v error=%v, want processed with no error", processed, processErr)
	}
}

// Razorpay retries until it gets a 2xx. The second delivery must change nothing.
func TestReplayedEventChangesNothing(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := testService(pool)
	ctx := context.Background()

	body := capturedEvent(f.providerOrderID, "pay_test_replay", f.total, "INR")
	eventID := "evt_" + uuid.NewString()

	if err := svc.Handle(ctx, body, sign(body), eventID); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	first := readState(t, pool, f)

	if err := svc.Handle(ctx, body, sign(body), eventID); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	second := readState(t, pool, f)

	if second.payments != 1 || second.entitlements != 1 {
		t.Errorf("after replay: payments=%d entitlements=%d, want 1 and 1", second.payments, second.entitlements)
	}
	// paid_at must not move: the order was settled by the first delivery, and a
	// retry is not a second purchase.
	if first.paidAt == nil || second.paidAt == nil || !first.paidAt.Equal(*second.paidAt) {
		t.Errorf("paid_at moved on replay: %v -> %v", first.paidAt, second.paidAt)
	}
}

// A fresh event id carrying the same payment must also not settle twice — this
// is the money-level guard (unique provider_payment_id), independent of the
// delivery-level one.
func TestSamePaymentUnderNewEventIDDoesNotDoubleRecord(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := testService(pool)
	ctx := context.Background()

	body := capturedEvent(f.providerOrderID, "pay_test_same", f.total, "INR")

	for i := 0; i < 2; i++ {
		if err := svc.Handle(ctx, body, sign(body), "evt_"+uuid.NewString()); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	s := readState(t, pool, f)
	if s.payments != 1 {
		t.Errorf("payments = %d, want 1 — provider_payment_id is unique for exactly this case", s.payments)
	}
	if s.entitlements != 1 {
		t.Errorf("entitlements = %d, want 1", s.entitlements)
	}
}

// The check that makes a public webhook safe: pay 1 rupee for a 499 rupee
// product and nothing is delivered.
func TestAmountMismatchDoesNotSettle(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := testService(pool)

	body := capturedEvent(f.providerOrderID, "pay_test_cheap", 100, "INR")
	eventID := "evt_" + uuid.NewString()

	// Acknowledged, not errored: retrying would produce the same refusal.
	if err := svc.Handle(context.Background(), body, sign(body), eventID); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	s := readState(t, pool, f)
	if s.status != "pending" {
		t.Errorf("order = %s, want pending", s.status)
	}
	if s.entitlements != 0 {
		t.Errorf("entitlements = %d, want 0 — nothing is delivered for the wrong amount", s.entitlements)
	}
	if s.payments != 0 {
		t.Errorf("payments = %d, want 0", s.payments)
	}

	processed, processErr := webhookError(t, pool, eventID)
	if !processed || processErr == nil {
		t.Fatalf("webhook processed=%v error=%v, want processed with a recorded reason", processed, processErr)
	}
	if !strings.Contains(*processErr, "amount mismatch") {
		t.Errorf("process_error = %q, want it to name the mismatch", *processErr)
	}
}

// A forged request must change nothing and leave no row — the table is not
// something an unauthenticated caller can write to.
func TestInvalidSignatureIsRejectedAndStoresNothing(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := testService(pool)

	body := capturedEvent(f.providerOrderID, "pay_forged", f.total, "INR")
	eventID := "evt_" + uuid.NewString()

	err := svc.Handle(context.Background(), body, "not-a-real-signature", eventID)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v, want ErrInvalidSignature", err)
	}

	s := readState(t, pool, f)
	if s.status != "pending" || s.payments != 0 || s.entitlements != 0 {
		t.Errorf("forged event changed state: %+v", s)
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_webhooks WHERE provider_event_id = $1`, eventID).Scan(&rows); err != nil {
		t.Fatalf("count webhooks: %v", err)
	}
	if rows != 0 {
		t.Errorf("payment_webhooks rows = %d, want 0 for a request that failed verification", rows)
	}
}

// An event for an order we have no record of is acknowledged and flagged, not
// retried forever.
func TestUnknownProviderOrderIsRecordedNotRetried(t *testing.T) {
	pool := testPool(t)
	svc := testService(pool)

	body := capturedEvent("order_does_not_exist_"+uuid.NewString()[:8], "pay_orphan", 49900, "INR")
	eventID := "evt_" + uuid.NewString()

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM payment_webhooks WHERE provider_event_id = $1`, eventID)
	})

	if err := svc.Handle(context.Background(), body, sign(body), eventID); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	processed, processErr := webhookError(t, pool, eventID)
	if !processed || processErr == nil {
		t.Fatalf("webhook processed=%v error=%v, want processed with a reason", processed, processErr)
	}
	if !strings.Contains(*processErr, "no local order") {
		t.Errorf("process_error = %q, want it to say no local order matched", *processErr)
	}
}

// A declined card is history, not a verdict on the order: the buyer can still
// pay with another card against the same pending order.
func TestFailedPaymentRecordsAttemptWithoutTouchingOrder(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	svc := testService(pool)

	body := []byte(fmt.Sprintf(
		`{"entity":"event","event":"payment.failed","payload":{"payment":{"entity":`+
			`{"id":"pay_failed_1","order_id":%q,"amount":%d,"currency":"INR","status":"failed",`+
			`"error_code":"BAD_REQUEST_ERROR","error_description":"card declined"}}}}`,
		f.providerOrderID, f.total))

	if err := svc.Handle(context.Background(), body, sign(body), "evt_"+uuid.NewString()); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	s := readState(t, pool, f)
	if s.status != "pending" {
		t.Errorf("order = %s, want pending — a failed attempt does not fail the order", s.status)
	}
	if s.payments != 1 {
		t.Errorf("payments = %d, want 1 (the failed attempt is kept)", s.payments)
	}
	if s.entitlements != 0 {
		t.Errorf("entitlements = %d, want 0", s.entitlements)
	}

	var status string
	var errCode *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, error_code FROM payments WHERE order_id = $1`, f.orderID).Scan(&status, &errCode); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if status != "failed" || errCode == nil {
		t.Errorf("payment = %s (code %v), want failed with the provider's reason kept", status, errCode)
	}
}
