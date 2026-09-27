package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func headersFor(t *testing.T, production bool) map[string]string {
	t.Helper()

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(SecurityHeaders(production))
	app.Get("/x", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/x", nil), 5000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	out := map[string]string{}
	for k := range resp.Header {
		out[k] = resp.Header.Get(k)
	}
	return out
}

func TestSecurityHeadersAreSet(t *testing.T) {
	got := headersFor(t, false)

	want := map[string]string{
		// The one that actually matters for a JSON API: without it a browser may
		// sniff past the declared Content-Type and treat a response as HTML.
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}

	for header, value := range want {
		if got[header] != value {
			t.Errorf("%s = %q, want %q", header, got[header], value)
		}
	}

	if csp := got["Content-Security-Policy"]; csp == "" {
		t.Error("Content-Security-Policy is missing")
	}
	if pp := got["Permissions-Policy"]; pp == "" {
		t.Error("Permissions-Policy is missing")
	}
}

// HSTS from a development server would teach the browser to refuse http on
// localhost — for two years, with no easy undo.
func TestHSTSOnlyInProduction(t *testing.T) {
	if hsts := headersFor(t, false)["Strict-Transport-Security"]; hsts != "" {
		t.Errorf("HSTS sent in development: %q", hsts)
	}

	hsts := headersFor(t, true)["Strict-Transport-Security"]
	if hsts == "" {
		t.Fatal("HSTS missing in production")
	}
	if !contains(hsts, "max-age=") || !contains(hsts, "includeSubDomains") {
		t.Errorf("HSTS = %q, want a max-age covering subdomains", hsts)
	}
}

// The legacy XSS filter is removed from modern browsers and its own
// implementation introduced vulnerabilities, so sending it is cargo cult.
func TestLegacyXSSHeaderIsNotSent(t *testing.T) {
	if v := headersFor(t, true)["X-Xss-Protection"]; v != "" {
		t.Errorf("X-XSS-Protection = %q, want it absent", v)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
