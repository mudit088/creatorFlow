package middleware

import (
	"github.com/gofiber/fiber/v2"
)

// SecurityHeaders sets the response headers that cost nothing and remove whole
// categories of attack.
//
// This API returns JSON, never HTML, which changes what is worth sending. The
// headers below are chosen for that: the ones that matter for a document are
// mostly irrelevant here, and the ones that matter here are about what a browser
// does when a response is *not* what it expected.
//
// production controls only HSTS, which must not be sent from a development
// server on plain http — a browser that sees it once will refuse to talk to
// localhost over http for a year, and there is no easy undo.
func SecurityHeaders(production bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// The single most valuable header here. Without it, a browser may ignore
		// the declared Content-Type and sniff the body: an endpoint that reflects
		// user input and is served as application/json can be coaxed into being
		// treated as HTML, and then it runs as HTML. nosniff makes the declared
		// type binding.
		c.Set("X-Content-Type-Options", "nosniff")

		// Nothing here is meant to be framed. Clickjacking against a JSON API is
		// not the obvious risk, but a future endpoint that returns a page is, and
		// this is one header rather than a decision to remember later.
		c.Set("X-Frame-Options", "DENY")

		// frame-ancestors is the modern half of the line above, and default-src
		// 'none' means that if a response is ever rendered as a document — by a
		// browser that was tricked, or by a future HTML endpoint — it may load
		// nothing at all: no scripts, no images, no fetches.
		c.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")

		// URLs in this API contain order ids, usernames and product slugs. Those
		// should not travel to a third party in a Referer header because someone
		// clicked a link, so none is sent at all.
		c.Set("Referrer-Policy", "no-referrer")

		// An API has no use for a camera, a microphone or a location, and saying
		// so means an embedded context cannot ask on our behalf.
		c.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")

		// Deliberately NOT set: X-XSS-Protection. It controls a legacy filter that
		// modern browsers removed, and its own implementation introduced
		// vulnerabilities — setting it now is cargo cult, and "1; mode=block" can
		// still change behaviour in old browsers for the worse.

		if production {
			// Two years, subdomains included, and eligible for preloading. Only in
			// production: this instructs a browser never to use http for this host
			// again, which is exactly right for a live domain and painful for a
			// developer whose localhost is http.
			c.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		}

		return c.Next()
	}
}
