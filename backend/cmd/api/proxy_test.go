package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// X-Forwarded-For is written by whoever sent the request. Believing it without
// knowing the sender is a proxy turns every per-IP protection in this API into
// decoration: one script can present a new address per request, never be rate
// limited, and make every visitor look unique.
//
// These tests pin both halves of the rule — ignored from an untrusted peer,
// honoured from a trusted one — because the failure is silent. Nothing breaks;
// the limits simply stop working.
//
// They run against a real TCP listener rather than fiber's app.Test, which uses
// a fake connection whose peer address is always 0.0.0.0. With no real peer
// there is no trust decision to make, so the fake would pass whatever the code
// did.

// serve starts the app on a loopback port and returns its base URL.
func serve(t *testing.T, trusted []string) string {
	t.Helper()

	trust := len(trusted) > 0
	app := fiber.New(fiber.Config{
		DisableStartupMessage:   true,
		EnableTrustedProxyCheck: trust,
		TrustedProxies:          trusted,
		ProxyHeader:             proxyHeader(trust),
	})
	app.Get("/whoami", func(c *fiber.Ctx) error { return c.SendString(c.IP()) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	return "http://" + ln.Addr().String()
}

func clientIP(t *testing.T, baseURL, forwardedFor string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/whoami", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// The default, and the one that matters most: with no proxy configured a forged
// header is ignored and the real peer is used.
func TestForwardedHeaderIgnoredWithoutTrustedProxy(t *testing.T) {
	url := serve(t, nil)

	got := clientIP(t, url, "203.0.113.99")
	if got == "203.0.113.99" {
		t.Fatal("a spoofed X-Forwarded-For was believed with no trusted proxy configured")
	}
	if got != "127.0.0.1" {
		t.Errorf("c.IP() = %q, want the peer address 127.0.0.1", got)
	}
}

// Behind a configured balancer the header carries the visitor's address, which
// is the entire reason for the setting.
func TestForwardedHeaderUsedFromTrustedProxy(t *testing.T) {
	url := serve(t, []string{"127.0.0.1"})

	if got := clientIP(t, url, "203.0.113.99"); got != "203.0.113.99" {
		t.Errorf("c.IP() = %q, want the forwarded client address 203.0.113.99", got)
	}
}

// A peer outside the trusted list is still not believed, even though other
// peers are trusted — otherwise configuring one proxy would trust the internet.
func TestForwardedHeaderIgnoredFromUntrustedPeer(t *testing.T) {
	url := serve(t, []string{"10.0.0.0/16"}) // a VPC range loopback is not in

	if got := clientIP(t, url, "203.0.113.99"); got != "127.0.0.1" {
		t.Errorf("c.IP() = %q, want the peer address — this peer is not a trusted proxy", got)
	}
}

// Without a trusted proxy the header must not even be consulted: setting
// ProxyHeader anyway makes Fiber read it regardless and defeats the check.
func TestProxyHeaderOnlySetWhenTrusting(t *testing.T) {
	if h := proxyHeader(false); h != "" {
		t.Errorf("proxyHeader(false) = %q, want empty", h)
	}
	if h := proxyHeader(true); h != fiber.HeaderXForwardedFor {
		t.Errorf("proxyHeader(true) = %q, want X-Forwarded-For", h)
	}
}
