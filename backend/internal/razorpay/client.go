// Package razorpay wraps the two pieces of Razorpay this project actually uses:
// creating an order, and proving an inbound webhook came from Razorpay.
//
// It is hand-written against the HTTP API rather than built on the official SDK.
// The whole surface is one POST and one HMAC comparison, and writing it means
// the signature verification below is something you can read and defend line by
// line instead of a function call you trust. The cost is honest: if Razorpay
// changes a field name, nothing tells us until a request fails.
package razorpay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mudit/creatorflow/backend/internal/config"
)

const defaultBaseURL = "https://api.razorpay.com"

type Client struct {
	baseURL       string
	keyID         string
	keySecret     string
	webhookSecret string
	http          *http.Client
}

func New(cfg *config.Config) *Client {
	return &Client{
		baseURL:       defaultBaseURL,
		keyID:         cfg.RazorpayKeyID,
		keySecret:     cfg.RazorpayKeySecret,
		webhookSecret: cfg.RazorpayWebhookSecret,
		// An explicit timeout, because http.DefaultClient has none. A payment
		// provider is a third party on the far side of the internet: without
		// this, one hung TCP connection holds a request, a pgx connection and a
		// goroutine open until something else gives up first.
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether real API calls are possible. Local development runs
// without keys, and the checkout flow degrades to "order created, payment not
// takeable" rather than failing — which keeps the rest of the system testable
// on a laptop with no Razorpay account.
func (c *Client) Enabled() bool { return c.keyID != "" && c.keySecret != "" }

// KeyID is safe to hand to a browser: it is the public half of the pair and
// Razorpay's own checkout script requires it. KeySecret never leaves this
// process.
func (c *Client) KeyID() string { return c.keyID }

// Order is the subset of Razorpay's order object worth keeping. The full
// response has a dozen more fields; mapping only what we use means their
// additions cannot break our parsing.
type Order struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Receipt  string `json:"receipt"`
	Status   string `json:"status"`
}

// APIError is a rejection Razorpay explained. Kept distinct from a transport
// failure because the two need opposite responses: this one means the request
// was wrong and retrying it unchanged will fail identically.
type APIError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("razorpay: %d %s: %s", e.StatusCode, e.Code, e.Description)
}

// CreateOrder registers the amount with Razorpay and returns the order the
// browser's checkout will reference.
//
// amountMinor is paise, which is what Razorpay expects for INR — the same unit
// this project stores in bigint columns, so no conversion happens anywhere and
// there is no place for a rounding error to enter.
//
// receipt carries our own order id. It is not decoration: when a payment is
// disputed months later, it is the field that ties Razorpay's dashboard row back
// to a row in our database without a human guessing from timestamps.
//
// Deliberately not retried. Razorpay does not deduplicate orders by receipt, so
// a blind retry of a request that actually succeeded but whose response was lost
// creates a second order for the same purchase. The caller retries instead, at a
// level where it can see that our order already has a provider id.
func (c *Client) CreateOrder(ctx context.Context, amountMinor int64, currency, receipt string, notes map[string]string) (*Order, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("razorpay: client is not configured")
	}

	payload := map[string]any{
		"amount":   amountMinor,
		"currency": currency,
		"receipt":  receipt,
	}
	if len(notes) > 0 {
		payload["notes"] = notes
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("razorpay: encode order request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/orders", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("razorpay: build order request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// HTTP Basic with the key pair. This is the key secret's only job; it is not
	// the webhook secret, and confusing the two produces an authentication
	// failure that reads like a code bug.
	req.SetBasicAuth(c.keyID, c.keySecret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("razorpay: create order: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read. A response body is attacker-adjacent input even from a
	// trusted vendor, and an unbounded io.ReadAll is how a process runs out of
	// memory because of something at the other end of a socket.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("razorpay: read order response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseAPIError(resp.StatusCode, raw)
	}

	var order Order
	if err := json.Unmarshal(raw, &order); err != nil {
		return nil, fmt.Errorf("razorpay: decode order response: %w", err)
	}
	if order.ID == "" {
		return nil, fmt.Errorf("razorpay: order response had no id")
	}
	return &order, nil
}

func parseAPIError(status int, raw []byte) error {
	var envelope struct {
		Error struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	}

	// An unparseable body still has to produce a useful error. A gateway between
	// us and Razorpay can return HTML, and "unexpected end of JSON input" would
	// hide the 502 that actually explains the failure.
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code == "" {
		return &APIError{StatusCode: status, Code: "unknown", Description: string(raw)}
	}
	return &APIError{
		StatusCode:  status,
		Code:        envelope.Error.Code,
		Description: envelope.Error.Description,
	}
}

// VerifyWebhookSignature proves an inbound request came from Razorpay.
//
// The signature is HMAC-SHA256 over the raw request body, keyed with the webhook
// secret, hex encoded, and sent in X-Razorpay-Signature. Three details matter:
//
//   - The body must be exactly the bytes received. Decoding to JSON and
//     re-encoding reorders keys and changes whitespace, and the hash of that is
//     not the hash Razorpay computed.
//   - hmac.Equal, never ==. String comparison returns early at the first
//     differing byte, so how long it takes leaks how much of a guess was
//     correct; an attacker can walk a forged signature out one byte at a time.
//     hmac.Equal takes the same time regardless.
//   - This is the webhook secret, not the key secret. They are different values
//     with different jobs: one authenticates us to Razorpay, this one
//     authenticates Razorpay to us.
//
// Without this check the endpoint is an unauthenticated "mark my order paid"
// button, which is the single most valuable thing anyone could find in this API.
func (c *Client) VerifyWebhookSignature(body []byte, signature string) bool {
	if c.webhookSecret == "" || signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}
