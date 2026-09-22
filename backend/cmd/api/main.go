package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/mudit/creatorflow/backend/internal/auth"
	"github.com/mudit/creatorflow/backend/internal/config"
	"github.com/mudit/creatorflow/backend/internal/database"
	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/middleware"
	"github.com/mudit/creatorflow/backend/internal/products"
	"github.com/mudit/creatorflow/backend/internal/profiles"
	"github.com/mudit/creatorflow/backend/internal/storage"
	"github.com/mudit/creatorflow/backend/internal/storefront"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.NewPostgres(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	cache, err := database.NewRedis(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	app, err := newServer(cfg, pool, cache)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "port", cfg.Port, "env", cfg.AppEnv)
		errCh <- app.Listen(":" + cfg.Port)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Stop accepting new connections, let in-flight requests finish. Without
		// this, a deploy can kill a request between "payment captured" and
		// "entitlement written".
		logger.Info("shutdown signal received, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return app.ShutdownWithContext(shutdownCtx)
	}
}

func newServer(cfg *config.Config, pool *pgxpool.Pool, cache *redis.Client) (*fiber.App, error) {
	app := fiber.New(fiber.Config{
		AppName:               "creatorflow-api",
		ErrorHandler:          httpx.ErrorHandler,
		DisableStartupMessage: true,
		ReadTimeout:           10 * time.Second,
		WriteTimeout:          20 * time.Second,
		IdleTimeout:           60 * time.Second,
		BodyLimit:             2 * 1024 * 1024, // large files go to S3, never through here
	})

	app.Use(recover.New())
	app.Use(middleware.RequestID())
	app.Use(cors.New(cors.Config{
		AllowOrigins:     joinOrigins(cfg.CORSAllowedOrigins),
		AllowMethods:     "GET,POST,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Content-Type,Authorization,X-Request-ID,Idempotency-Key",
		AllowCredentials: true,
	}))

	// Liveness: is the process up? Deliberately checks nothing else — if this
	// touched the database, a brief database blip would make the orchestrator
	// kill healthy API containers and turn a small outage into a large one.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "env": cfg.AppEnv})
	})

	// Readiness: should this instance receive traffic? This one does check
	// dependencies, because an instance that cannot reach PostgreSQL should be
	// pulled from the load balancer rather than serving errors.
	app.Get("/ready", func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()

		checks := fiber.Map{"postgres": "ok", "redis": "ok"}
		ready := true

		if err := pool.Ping(ctx); err != nil {
			checks["postgres"] = "unavailable"
			ready = false
		}
		if err := cache.Ping(ctx).Err(); err != nil {
			// Redis is a cache, not a source of truth — degraded, not down.
			checks["redis"] = "degraded"
		}

		status := fiber.StatusOK
		if !ready {
			status = fiber.StatusServiceUnavailable
		}
		return c.Status(status).JSON(fiber.Map{"ready": ready, "checks": checks})
	})

	// Versioned from day one. Adding /api/v2 later must not require breaking v1.
	v1 := app.Group("/api/v1")
	v1.Get("/ping", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"pong": true, "request_id": middleware.FromContext(c)})
	})

	// One issuer for the whole process. It is the only thing that holds the
	// signing secret, and both the auth service (which mints tokens) and the
	// middleware (which verifies them) are handed the same instance — so a
	// rotated secret can never be half-applied.
	tokens := auth.NewTokenIssuer(cfg.JWTAccessSecret, cfg.JWTRefreshSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)

	authSvc, err := auth.NewService(auth.NewRepository(pool), tokens)
	if err != nil {
		return nil, err
	}

	// Constructed here, once, so this file is the only place you need to read to
	// know which routes require a valid access token.
	requireAuth := middleware.RequireAuth(tokens, httpx.ErrUnauthorized)

	auth.NewHandler(authSvc, auth.CookieConfig{
		// Scoped to the auth routes: the refresh cookie is never attached to a
		// product, storefront or payment request, so it cannot leak through one.
		Path: "/api/v1/auth",
		// http://localhost cannot set a Secure cookie, and production must never
		// send this one over anything but TLS.
		Secure: cfg.IsProduction(),
		MaxAge: cfg.RefreshTokenTTL,
	}).RegisterRoutes(v1, requireAuth)

	profiles.NewHandler(profiles.NewService(profiles.NewRepository(pool))).
		RegisterRoutes(v1, requireAuth)

	// One S3 client for the process. It holds credentials and connection reuse,
	// so building one per request would be both slower and a way to leak file
	// descriptors under load.
	objectStore := storage.New(cfg)

	products.NewHandler(products.NewService(products.NewRepository(pool), objectStore)).
		RegisterRoutes(v1, requireAuth)

	// The public storefront. No requireAuth here, deliberately and visibly: this
	// is the one group of routes anyone on the internet can call, so it is worth
	// being able to see that at a glance in this file.
	storefront.NewHandler(storefront.NewService(storefront.NewRepository(pool))).
		RegisterRoutes(v1)

	return app, nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	// JSON in production so CloudWatch can index fields; text locally so you can
	// actually read it.
	if cfg.IsProduction() {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func joinOrigins(origins []string) string {
	out := ""
	for i, o := range origins {
		if i > 0 {
			out += ","
		}
		out += o
	}
	return out
}
