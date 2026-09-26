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
	"github.com/mudit/creatorflow/backend/internal/delivery"
	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/middleware"
	"github.com/mudit/creatorflow/backend/internal/orders"
	"github.com/mudit/creatorflow/backend/internal/payments"
	"github.com/mudit/creatorflow/backend/internal/products"
	"github.com/mudit/creatorflow/backend/internal/profiles"
	"github.com/mudit/creatorflow/backend/internal/razorpay"
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
		AllowOrigins: joinOrigins(cfg.CORSAllowedOrigins),
		// The API speaks GET and POST only, so nothing else is advertised.
		// Narrowing this is not cosmetic: a browser preflight asks what is
		// allowed, and answering with verbs no route accepts invites a client to
		// send a request that can only ever 405.
		AllowMethods:     "GET,POST,OPTIONS",
		AllowHeaders:     "Content-Type,Authorization,X-Request-ID,Idempotency-Key",
		AllowCredentials: true,
	}))

	// One issuer for the whole process. It is the only thing that holds the
	// signing secret, and both the auth service (which mints tokens) and the
	// middleware (which verifies them) are handed the same instance — so a
	// rotated secret can never be half-applied.
	tokens := auth.NewTokenIssuer(cfg.JWTAccessSecret, cfg.JWTRefreshSecret, cfg.AccessTokenTTL, cfg.RefreshTokenTTL)

	authSvc, err := auth.NewService(auth.NewRepository(pool), tokens)
	if err != nil {
		return nil, err
	}

	// One S3 client for the process. It holds credentials and connection reuse,
	// so building one per request would be both slower and a way to leak file
	// descriptors under load.
	objectStore := storage.New(cfg)

	// One Razorpay client for the process, holding both secrets. When the keys
	// are absent — the normal state on a laptop — it reports itself disabled and
	// checkout still works end to end, minus the payment itself.
	razorpayClient := razorpay.New(cfg)
	if !razorpayClient.Enabled() {
		slog.Warn("razorpay is not configured: orders will be created without a provider order and cannot be paid")
	}

	live, ready := newProbes(cfg.AppEnv,
		pool.Ping,
		func(ctx context.Context) error { return cache.Ping(ctx).Err() },
	)

	// Construction happens here; the URL-to-handler mapping lives in routes.go.
	registerRoutes(app, handlers{
		auth: auth.NewHandler(authSvc, auth.CookieConfig{
			// Scoped to the auth routes: the refresh cookie is never attached to
			// a product, storefront or payment request, so it cannot leak
			// through one.
			Path: "/api/v1/auth",
			// http://localhost cannot set a Secure cookie, and production must
			// never send this one over anything but TLS.
			Secure: cfg.IsProduction(),
			MaxAge: cfg.RefreshTokenTTL,
		}),
		profiles:   profiles.NewHandler(profiles.NewService(profiles.NewRepository(pool))),
		products:   products.NewHandler(products.NewService(products.NewRepository(pool), objectStore)),
		storefront: storefront.NewHandler(storefront.NewService(storefront.NewRepository(pool))),
		orders:     orders.NewHandler(orders.NewService(orders.NewRepository(pool), razorpayClient)),
		payments:   payments.NewHandler(payments.NewService(payments.NewRepository(pool), razorpayClient)),
		delivery:   delivery.NewHandler(delivery.NewService(delivery.NewRepository(pool), objectStore)),

		requireAuth: middleware.RequireAuth(tokens, httpx.ErrUnauthorized),
		live:        live,
		ready:       ready,
	})

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
