package main

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/mudit/creatorflow/backend/internal/auth"
	"github.com/mudit/creatorflow/backend/internal/delivery"
	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/middleware"
	"github.com/mudit/creatorflow/backend/internal/orders"
	"github.com/mudit/creatorflow/backend/internal/payments"
	"github.com/mudit/creatorflow/backend/internal/products"
	"github.com/mudit/creatorflow/backend/internal/profiles"
	"github.com/mudit/creatorflow/backend/internal/storefront"
)

// This file is the whole HTTP surface of the API. Every path the process will
// answer is written below, once, in one list.
//
// The tradeoff, since it is a real one: each feature package no longer owns its
// own routes, and its handler methods had to become exported for this file to
// reference them. What that buys is that "which endpoints are public?" and "what
// can an unauthenticated caller reach?" are answered by reading one screen
// rather than by opening five handler files and trusting that none of them
// registered something quietly. For an API whose security depends on which
// routes carry requireAuth, that question is worth optimising for.
//
// main.go keeps construction — pools, config, services. This file keeps only the
// mapping from URL to handler.

// handlers is every dependency the route table needs, assembled in main.go. It
// is a struct rather than a long parameter list so that adding a feature package
// does not change this function's signature.
type handlers struct {
	auth       *auth.Handler
	profiles   *profiles.Handler
	products   *products.Handler
	storefront *storefront.Handler
	orders     *orders.Handler
	delivery   *delivery.Handler
	payments   *payments.Handler

	requireAuth fiber.Handler
	// probes are closures over the pool and cache, built in main.go, so this
	// file needs no database imports.
	live  fiber.Handler
	ready fiber.Handler
}

// registerRoutes mounts everything. Read top to bottom: process probes, then the
// versioned API, grouped by who is allowed to call it.
//
// The API speaks GET and POST only. Reads are GET; anything that changes state
// is a POST whose path names the action. The cost is that POST /x/delete is not
// idempotent the way DELETE is, so a client retrying a timed-out request has no
// protocol-level guarantee it is safe. What it buys is a surface every client,
// proxy and HTML form can reach with no verb negotiation.
func registerRoutes(app *fiber.App, h handlers) {
	// ---------------------------------------------------------------- probes
	// Outside /api/v1 on purpose: these describe the process, not the product,
	// and an orchestrator should not have to know the API's version to check
	// whether the container is alive.
	app.Get("/health", h.live)
	app.Get("/ready", h.ready)

	// ------------------------------------------------------------------- v1
	// Versioned from day one. Adding /api/v2 later must not require breaking v1.
	v1 := app.Group("/api/v1")
	v1.Get("/ping", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"pong": true, "request_id": middleware.FromContext(c)})
	})

	// ------------------------------------------------------------------ auth
	// Public: these are how a caller obtains credentials in the first place.
	v1.Post("/auth/register", h.auth.Register)
	v1.Post("/auth/login", h.auth.Login)
	v1.Post("/auth/refresh", h.auth.Refresh)
	v1.Post("/auth/logout", h.auth.Logout)

	// --------------------------------------------------------------- creator
	// Everything below requires a valid access token. The middleware is attached
	// per route rather than per group so that this list can be read without
	// tracking which group a line belongs to: if a route does not name
	// requireAuth, it is public.
	v1.Get("/me", h.requireAuth, h.auth.Me)

	// Owner routes address /me rather than /profiles/:id. With an id in the path
	// every handler has to remember to check that the id belongs to the caller,
	// and the day one forgets is the day anyone can edit anyone's page. With /me
	// the id comes from the verified token and is never client-supplied, so that
	// entire class of authorization bug cannot be written. Links nest under it
	// for the same reason: the URL states the ownership the SQL then enforces.
	v1.Post("/profiles", h.requireAuth, h.profiles.Create)
	v1.Get("/profiles/me", h.requireAuth, h.profiles.Own)
	v1.Post("/profiles/me/update", h.requireAuth, h.profiles.Update)
	v1.Post("/profiles/me/links", h.requireAuth, h.profiles.AddLink)
	v1.Post("/profiles/me/links/order", h.requireAuth, h.profiles.Reorder)
	v1.Post("/profiles/me/links/:id/update", h.requireAuth, h.profiles.UpdateLink)
	v1.Post("/profiles/me/links/:id/delete", h.requireAuth, h.profiles.DeleteLink)

	v1.Post("/products", h.requireAuth, h.products.Create)
	v1.Get("/products", h.requireAuth, h.products.List)
	v1.Get("/products/:id", h.requireAuth, h.products.Get)
	v1.Post("/products/:id/update", h.requireAuth, h.products.Update)

	// None of the upload routes carry a file body: the API hands out a signed URL
	// and later asks S3 what happened. A 2 GB product never touches this process,
	// which is what keeps its memory flat and its BodyLimit at 2 MB.
	v1.Post("/products/:id/upload-url", h.requireAuth, h.products.RequestUpload)
	v1.Post("/products/:id/files/:fileId/confirm", h.requireAuth, h.products.ConfirmUpload)
	v1.Post("/products/:id/files/:fileId/delete", h.requireAuth, h.products.DeleteFile)

	// ---------------------------------------------------------------- public
	// The rest of the internet. Everything below is deliberately unauthenticated
	// and should be read as such: these four routes are the entire attack
	// surface available without credentials.
	//
	// The storefront is world-readable by design, which is why it returns its own
	// response types rather than the owner-facing ones.
	v1.Get("/public/:username", h.storefront.Page)
	v1.Get("/public/:username/:slug", h.storefront.Product)

	// Checkout is public because buyers are guests: putting an account between a
	// creator's audience and their Buy button is the fastest way to lose a sale.
	// It is also the only unauthenticated route that writes, so it is the first
	// one that needs per-IP rate limiting — which arrives with Redis in phase 11.
	// Until then it is bounded only by maxItemsPerOrder and the body limit.
	v1.Post("/orders", h.orders.Create)

	// Secure delivery. Public because buyers are guests, and protected by two
	// things the caller must already have: the order's unguessable uuid and the
	// email that bought it. It hands back short-lived presigned S3 URLs — the
	// file itself never passes through this process, in either direction.
	v1.Post("/orders/:id/download", h.delivery.Download)

	// The payment webhook. Unauthenticated in the usual sense — Razorpay holds no
	// token of ours — but not unauthenticated in effect: every request is
	// verified by HMAC over its raw body before anything reads it. That check is
	// the only thing standing between this URL and a "mark my order paid" button,
	// which makes it the most security-critical line in this file.
	v1.Post("/payments/webhook", h.payments.Webhook)
}

// newProbes builds the liveness and readiness handlers.
//
// Liveness checks nothing but the process. If it touched the database, a brief
// database blip would make the orchestrator kill healthy API containers and turn
// a small outage into a large one.
//
// Readiness does check dependencies, because an instance that cannot reach
// PostgreSQL should be pulled from the load balancer rather than serve errors.
// Redis failing is degraded, not down: it is a cache, not a source of truth.
func newProbes(env string, pingDB, pingCache func(context.Context) error) (live, ready fiber.Handler) {
	live = func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "env": env})
	}

	ready = func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()

		checks := fiber.Map{"postgres": "ok", "redis": "ok"}
		isReady := true

		if err := pingDB(ctx); err != nil {
			checks["postgres"] = "unavailable"
			isReady = false
		}
		if err := pingCache(ctx); err != nil {
			checks["redis"] = "degraded"
		}

		status := fiber.StatusOK
		if !isReady {
			status = fiber.StatusServiceUnavailable
		}
		return c.Status(status).JSON(fiber.Map{"ready": isReady, "checks": checks})
	}

	return live, ready
}

// compile-time assurance that the error shape stays wired to Fiber's handler.
var _ fiber.ErrorHandler = httpx.ErrorHandler
