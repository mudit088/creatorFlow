package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every value the process needs to run. It is loaded once at
// startup and passed explicitly to the things that need it. Nothing in this
// codebase reads os.Getenv outside of this file — that keeps configuration
// testable and makes the full surface of required env vars visible in one place.
type Config struct {
	AppEnv   string
	LogLevel string
	Port     string

	DatabaseURL string
	DBMaxConns  int32
	DBMinConns  int32

	RedisURL string

	S3Endpoint       string
	S3Region         string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string
	S3ForcePathStyle bool

	JWTAccessSecret  string
	JWTRefreshSecret string
	AccessTokenTTL   time.Duration
	RefreshTokenTTL  time.Duration

	RazorpayKeyID         string
	RazorpayKeySecret     string
	RazorpayWebhookSecret string

	CORSAllowedOrigins []string
}

func (c Config) IsProduction() bool { return c.AppEnv == "production" }

// Load reads configuration from the environment and fails loudly if anything
// required is missing. A process that starts with half its configuration is
// worse than one that refuses to start.
func Load() (*Config, error) {
	var missing []string

	req := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	cfg := &Config{
		AppEnv:   optional("APP_ENV", "development"),
		LogLevel: optional("LOG_LEVEL", "info"),
		Port:     optional("API_PORT", "8080"),

		DatabaseURL: req("DATABASE_URL"),
		DBMaxConns:  int32(optionalInt("DB_MAX_CONNS", 10)),
		DBMinConns:  int32(optionalInt("DB_MIN_CONNS", 2)),

		RedisURL: req("REDIS_URL"),

		S3Endpoint:       optional("S3_ENDPOINT", ""),
		S3Region:         optional("S3_REGION", "ap-south-1"),
		S3Bucket:         req("S3_BUCKET"),
		S3AccessKey:      req("S3_ACCESS_KEY"),
		S3SecretKey:      req("S3_SECRET_KEY"),
		S3ForcePathStyle: optionalBool("S3_FORCE_PATH_STYLE", false),

		JWTAccessSecret:  req("JWT_ACCESS_SECRET"),
		JWTRefreshSecret: req("JWT_REFRESH_SECRET"),
		AccessTokenTTL:   optionalDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:  optionalDuration("REFRESH_TOKEN_TTL", 720*time.Hour),

		RazorpayKeyID:         optional("RAZORPAY_KEY_ID", ""),
		RazorpayKeySecret:     optional("RAZORPAY_KEY_SECRET", ""),
		RazorpayWebhookSecret: optional("RAZORPAY_WEBHOOK_SECRET", ""),

		CORSAllowedOrigins: splitCSV(optional("CORS_ALLOWED_ORIGINS", "http://localhost:3000")),
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// Guardrails that only matter once real money and real users are involved.
	if cfg.IsProduction() {
		if strings.Contains(cfg.JWTAccessSecret, "replace-me") {
			return nil, fmt.Errorf("refusing to start: JWT_ACCESS_SECRET still holds its placeholder value")
		}
		if cfg.RazorpayWebhookSecret == "" {
			return nil, fmt.Errorf("refusing to start: RAZORPAY_WEBHOOK_SECRET is required in production")
		}
	}

	return cfg, nil
}

func optional(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func optionalInt(key string, fallback int) int {
	if v, err := strconv.Atoi(optional(key, "")); err == nil {
		return v
	}
	return fallback
}

func optionalBool(key string, fallback bool) bool {
	if v, err := strconv.ParseBool(optional(key, "")); err == nil {
		return v
	}
	return fallback
}

func optionalDuration(key string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(optional(key, "")); err == nil {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
