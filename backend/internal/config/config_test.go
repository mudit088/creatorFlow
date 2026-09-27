package config

import (
	"strings"
	"testing"
)

// The production guards are the interesting part of this package: they are the
// difference between a misconfiguration being caught at startup and it being
// discovered by someone else later.

func setProductionEnv(t *testing.T) {
	t.Helper()

	// A complete, valid production environment. Each test then breaks exactly
	// one thing, so a failure names the guard that fired.
	for k, v := range map[string]string{
		"APP_ENV":                 "production",
		"DATABASE_URL":            "postgres://u:p@db:5432/cf?sslmode=require",
		"REDIS_URL":               "redis://cache:6379/0",
		"S3_BUCKET":               "creatorflow",
		"S3_ACCESS_KEY":           "key",
		"S3_SECRET_KEY":           "secret",
		"JWT_ACCESS_SECRET":       "a-real-access-secret-value",
		"JWT_REFRESH_SECRET":      "a-real-refresh-secret-value",
		"RAZORPAY_WEBHOOK_SECRET": "a-real-webhook-secret",
		"ANALYTICS_SALT":          "a-real-salt",
		"CORS_ALLOWED_ORIGINS":    "https://creatorflow.app",
	} {
		t.Setenv(k, v)
	}
}

func TestProductionConfigLoads(t *testing.T) {
	setProductionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("a complete production environment should load: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction() = false for APP_ENV=production")
	}
}

// The classic CORS hole: every site on the internet allowed to make credentialed
// requests. Browsers refuse the literal combination, but Fiber will echo the
// request's origin instead, which is the same hole with extra steps.
func TestProductionRefusesWildcardCORS(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("CORS_ALLOWED_ORIGINS", "*")

	_, err := Load()
	if err == nil {
		t.Fatal("expected a wildcard CORS origin to be refused in production")
	}
	if !strings.Contains(err.Error(), "CORS") {
		t.Errorf("error = %q, want it to name the CORS setting", err)
	}
}

// An http origin in production means credentials travelling in clear text.
func TestProductionRefusesInsecureCORSOrigin(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("CORS_ALLOWED_ORIGINS", "http://creatorflow.app")

	if _, err := Load(); err == nil {
		t.Fatal("expected a non-https CORS origin to be refused in production")
	}
}

// A placeholder signing secret in production means anyone who has read the
// repository can mint tokens.
func TestProductionRefusesPlaceholderSecret(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("JWT_ACCESS_SECRET", "replace-me-in-every-environment")

	if _, err := Load(); err == nil {
		t.Fatal("expected a placeholder JWT secret to be refused in production")
	}
}

// Without a webhook secret, signature verification fails closed and every real
// payment notification is rejected — so the process refuses to start instead.
func TestProductionRequiresWebhookSecret(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("RAZORPAY_WEBHOOK_SECRET", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected a missing webhook secret to be refused in production")
	}
}

// Without a stable salt each restart invents one, and the same visitor is
// counted again — inflated numbers that look plausible.
func TestProductionRequiresAnalyticsSalt(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("ANALYTICS_SALT", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected a missing analytics salt to be refused in production")
	}
}

// Development is deliberately permissive: none of the guards above apply, so a
// laptop with no Razorpay account and an http frontend still runs.
func TestDevelopmentAllowsLocalDefaults(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("APP_ENV", "development")
	t.Setenv("CORS_ALLOWED_ORIGINS", "http://localhost:3000")
	t.Setenv("RAZORPAY_WEBHOOK_SECRET", "")
	t.Setenv("ANALYTICS_SALT", "")

	if _, err := Load(); err != nil {
		t.Fatalf("development should tolerate local defaults: %v", err)
	}
}

// Missing variables are reported together, so fixing a deployment is one pass
// rather than one restart per variable.
func TestMissingRequiredVariablesAreReportedTogether(t *testing.T) {
	setProductionEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("S3_BUCKET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected missing variables to be refused")
	}
	for _, name := range []string{"DATABASE_URL", "REDIS_URL", "S3_BUCKET"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}
