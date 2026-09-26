package razorpay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests exist because the alternative is not testing this code at all:
// without Razorpay credentials there is nothing to point it at, and the two
// things worth getting right here — what we send, and whether we believe an
// inbound signature — are exactly the two things a live account would not check
// for us anyway.

func testClient(baseURL string) *Client {
	return &Client{
		baseURL:       baseURL,
		keyID:         "rzp_test_abc123",
		keySecret:     "key-secret",
		webhookSecret: "webhook-secret",
		http:          &http.Client{Timeout: 5 * time.Second},
	}
}

func TestCreateOrderSendsExpectedRequest(t *testing.T) {
	var (
		gotPath    string
		gotBody    map[string]any
		gotUser    string
		gotPass    string
		gotHasAuth bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, gotHasAuth = r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"order_ABC","amount":49900,"currency":"INR","receipt":"local-id","status":"created"}`))
	}))
	defer srv.Close()

	order, err := testClient(srv.URL).CreateOrder(context.Background(), 49900, "INR", "local-id", map[string]string{"order_id": "local-id"})
	if err != nil {
		t.Fatalf("CreateOrder returned error: %v", err)
	}

	if gotPath != "/v1/orders" {
		t.Errorf("path = %q, want /v1/orders", gotPath)
	}
	if !gotHasAuth || gotUser != "rzp_test_abc123" || gotPass != "key-secret" {
		t.Errorf("basic auth = (%q, %q, %v), want the key pair", gotUser, gotPass, gotHasAuth)
	}
	// The amount must cross the wire as paise, the same integer the database
	// holds. A float here would be the beginning of a rounding bug in a ledger.
	if amount, ok := gotBody["amount"].(float64); !ok || int64(amount) != 49900 {
		t.Errorf("amount = %v, want 49900", gotBody["amount"])
	}
	if gotBody["receipt"] != "local-id" {
		t.Errorf("receipt = %v, want local-id (our order id, for reconciliation)", gotBody["receipt"])
	}
	if order.ID != "order_ABC" || order.Amount != 49900 {
		t.Errorf("parsed order = %+v, want id order_ABC and amount 49900", order)
	}
}

func TestCreateOrderTranslatesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_REQUEST_ERROR","description":"amount must be at least 100"}}`))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).CreateOrder(context.Background(), 1, "INR", "local-id", nil)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "BAD_REQUEST_ERROR" {
		t.Errorf("error = %+v, want the 400 and its code preserved", apiErr)
	}
	if !strings.Contains(apiErr.Description, "at least 100") {
		t.Errorf("description = %q, want Razorpay's own explanation", apiErr.Description)
	}
}

// A gateway between us and Razorpay can answer with HTML. The failure has to
// stay legible instead of becoming "unexpected end of JSON input".
func TestCreateOrderHandlesNonJSONFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).CreateOrder(context.Background(), 49900, "INR", "local-id", nil)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || !strings.Contains(apiErr.Description, "502") {
		t.Errorf("error = %+v, want the 502 preserved", apiErr)
	}
}

// A 200 with no id is not a success. Treating it as one would store an empty
// provider order id and silently break reconciliation.
func TestCreateOrderRejectsResponseWithoutID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"amount":49900,"currency":"INR"}`))
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).CreateOrder(context.Background(), 49900, "INR", "local-id", nil); err == nil {
		t.Fatal("expected an error for a response with no order id")
	}
}

func TestCreateOrderRefusesWhenNotConfigured(t *testing.T) {
	c := &Client{baseURL: "http://unused", http: &http.Client{}}
	if c.Enabled() {
		t.Fatal("Enabled() = true with no keys")
	}
	if _, err := c.CreateOrder(context.Background(), 49900, "INR", "local-id", nil); err == nil {
		t.Fatal("expected an error when the client has no credentials")
	}
}

// The signature fixtures below were produced with the same algorithm Razorpay
// documents: hex(HMAC-SHA256(body, webhook_secret)).
func TestVerifyWebhookSignature(t *testing.T) {
	c := testClient("http://unused")
	body := []byte(`{"event":"payment.captured","payload":{}}`)

	// Generated for this exact body and the secret "webhook-secret".
	valid := hmacHex(t, "webhook-secret", body)

	cases := []struct {
		name      string
		body      []byte
		signature string
		want      bool
	}{
		{"valid signature", body, valid, true},
		{"empty signature", body, "", false},
		{"signature from a different secret", body, hmacHex(t, "attacker-secret", body), false},
		{"body altered after signing", []byte(`{"event":"payment.captured","payload":{"x":1}}`), valid, false},
		{"one byte flipped", body, "0" + valid[1:], false},
		{"truncated signature", body, valid[:16], false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.VerifyWebhookSignature(tc.body, tc.signature); got != tc.want {
				t.Errorf("VerifyWebhookSignature = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unconfigured webhook secret must fail closed. Returning true here would
// make every forged request valid on any deployment that forgot the variable.
func TestVerifyWebhookSignatureFailsClosedWithoutSecret(t *testing.T) {
	c := &Client{http: &http.Client{}}
	body := []byte(`{"event":"payment.captured"}`)
	if c.VerifyWebhookSignature(body, hmacHex(t, "", body)) {
		t.Fatal("verification passed with no webhook secret configured")
	}
}

// hmacHex computes what Razorpay would send, so the tests assert against the
// algorithm rather than against a hard-coded string nobody can re-derive.
func hmacHex(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
