package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/mudit/creatorflow/backend/internal/analytics"
	"github.com/mudit/creatorflow/backend/internal/auth"
	"github.com/mudit/creatorflow/backend/internal/cache"
	"github.com/mudit/creatorflow/backend/internal/config"
	"github.com/mudit/creatorflow/backend/internal/database"
	"github.com/mudit/creatorflow/backend/internal/delivery"
	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/jobs"
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

	// Analytics owns a worker goroutine draining a bounded queue. It is closed
	// after the HTTP server has drained — this defer runs once run() returns,
	// which is after ShutdownWithContext — so nothing new is queued while it
	// flushes what is left.
	analyticsSvc := analytics.NewService(analytics.NewRepository(pool), analyticsSalt(cfg))
	defer analyticsSvc.Close()

	// Periodic cleanup. Started before the server and stopped after it, so a job
	// mid-batch is allowed to finish rather than being killed by the shutdown
	// that drained the HTTP traffic.
	// One S3 client for the process: it holds credentials and pooled
	// connections, so both the request path and the cleanup job share it.
	objectStore := storage.New(cfg)

	cleanupRunner := startJobs(ctx, pool, cache, objectStore)
	defer cleanupRunner.Stop()

	app, err := newServer(cfg, pool, cache, analyticsSvc, objectStore)
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

// analyticsSalt returns the configured salt, or a random one for development.
//
// A per-process random salt means visitor counts reset on restart, which is
// wrong but harmless locally and strictly better than the alternative: a
// hard-coded default would be identical on every deployment that forgot to set
// one, and a known salt makes the visitor hashes reversible. Production refuses
// to start without a real value, so this branch is development-only by
// construction.
// startJobs registers the periodic work and launches it.
//
// The intervals are not synchronised with the thresholds they enforce: orders
// expire after 24 hours but the job looks every 10 minutes, because the cost of
// checking is one indexed query and the benefit is that a threshold change takes
// effect promptly rather than a day later.
func startJobs(ctx context.Context, pool *pgxpool.Pool, rdb *redis.Client, store *storage.Client) *jobs.Runner {
	cleanup := jobs.NewCleanup(pool)
	cleanup.DeleteObject = store.DeleteObject

	runner := jobs.New(rdb)
	runner.Add(jobs.Job{
		Name: "expire_pending_orders", Interval: 10 * time.Minute, Timeout: 2 * time.Minute,
		Run: cleanup.ExpirePendingOrders,
	})
	runner.Add(jobs.Job{
		Name: "sweep_abandoned_uploads", Interval: time.Hour, Timeout: 5 * time.Minute,
		Run: cleanup.SweepAbandonedUploads,
	})
	// Daily: retention is measured in months, so checking more often would be
	// work with nothing to find.
	runner.Add(jobs.Job{
		Name: "delete_expired_analytics", Interval: 24 * time.Hour, Timeout: 10 * time.Minute,
		Run: cleanup.DeleteExpiredAnalytics,
	})

	runner.Start(ctx)
	slog.Info("background jobs started", "jobs", 3)
	return runner
}

func analyticsSalt(cfg *config.Config) string {
	if cfg.AnalyticsSalt != "" {
		return cfg.AnalyticsSalt
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not a condition worth degrading for.
		panic("cannot generate analytics salt: " + err.Error())
	}
	slog.Warn("ANALYTICS_SALT is not set; using a random per-process salt, so visitor counts reset on restart")
	return hex.EncodeToString(buf)
}

func newServer(cfg *config.Config, pool *pgxpool.Pool, rdb *redis.Client, analyticsSvc *analytics.Service, objectStore *storage.Client) (*fiber.App, error) {
	// Behind a load balancer the peer address is the balancer, not the visitor.
	// Every rate limit bucket would then be shared by everyone, and the unique
	// visitor count would collapse to one.
	//
	// The fix is not "read X-Forwarded-For" — that header is written by the
	// client and can say anything, so believing it unconditionally is worse than
	// ignoring it: one script could present a new address per request and walk
	// through every limit here. Fiber is told which peers are allowed to set it,
	// and trusts the header only from them. With TRUSTED_PROXIES empty, as it is
	// locally, nothing is trusted and c.IP() stays the peer address.
	trustProxies := len(cfg.TrustedProxies) > 0

	app := fiber.New(fiber.Config{
		AppName:                 "creatorflow-api",
		EnableTrustedProxyCheck: trustProxies,
		TrustedProxies:          cfg.TrustedProxies,
		ProxyHeader:             proxyHeader(trustProxies),
		ErrorHandler:            httpx.ErrorHandler,
		DisableStartupMessage:   true,
		ReadTimeout:             10 * time.Second,
		WriteTimeout:            20 * time.Second,
		IdleTimeout:             60 * time.Second,
		BodyLimit:               2 * 1024 * 1024, // large files go to S3, never through here
	})

	app.Use(recover.New())
	app.Use(middleware.RequestID())
	// Before CORS and before any route: these apply to every response this
	// process produces, including the ones the error handler writes.
	app.Use(middleware.SecurityHeaders(cfg.IsProduction()))
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

	// One cache client. The storefront reads through it; profiles and products
	// write through its invalidation hooks, which is why it is built before them.
	pageCache := cache.New(rdb)

	// One Razorpay client for the process, holding both secrets. When the keys
	// are absent — the normal state on a laptop — it reports itself disabled and
	// checkout still works end to end, minus the payment itself.
	razorpayClient := razorpay.New(cfg)
	if !razorpayClient.Enabled() {
		slog.Warn("razorpay is not configured: orders will be created without a provider order and cannot be paid")
	}

	// Built once and shared: the storefront both serves cached pages and owns the
	// invalidation the writers call, so there is exactly one place that knows
	// how a cache key is spelled.
	storefrontSvc := storefront.NewService(storefront.NewRepository(pool), pageCache)

	live, ready := newProbes(cfg.AppEnv,
		pool.Ping,
		func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
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
		profiles:   profiles.NewHandler(profiles.NewService(profiles.NewRepository(pool), storefrontSvc)),
		products:   products.NewHandler(products.NewService(products.NewRepository(pool), objectStore, storefrontSvc)),
		storefront: storefront.NewHandler(storefrontSvc, analyticsSvc),
		orders:     orders.NewHandler(orders.NewService(orders.NewRepository(pool), razorpayClient)),
		payments:   payments.NewHandler(payments.NewService(payments.NewRepository(pool), razorpayClient)),
		delivery:   delivery.NewHandler(delivery.NewService(delivery.NewRepository(pool), objectStore)),
		analytics:  analytics.NewHandler(analyticsSvc),

		requireAuth: middleware.RequireAuth(tokens, httpx.ErrUnauthorized),
		// Backed by the same Redis the cache uses. It fails open, so a Redis
		// outage costs protection rather than availability.
		limiter: middleware.NewRateLimiter(rdb),
		live:    live,
		ready:   ready,
	})

	return app, nil
}

// proxyHeader returns the header Fiber should read the client address from, and
// only when a trusted proxy is configured. Returning it unconditionally would
// make the trust check pointless.
func proxyHeader(trustProxies bool) string {
	if !trustProxies {
		return ""
	}
	// X-Forwarded-For rather than X-Real-IP: it is what an AWS ALB, CloudFront
	// and nginx all set by default, and Fiber takes the left-most entry that a
	// trusted hop did not add.
	return fiber.HeaderXForwardedFor
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
