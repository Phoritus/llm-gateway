package config

import (
	"strings"
	"testing"
	"time"
)

// The variables a valid production environment needs. Each test starts from
// this and breaks one thing, so a test says what it is about by what it
// removes.
func productionEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"APP_ENV":         "production",
		"DATABASE_URL":    "postgres://gateway:gateway@db:5432/llm_gateway?sslmode=require",
		"REDIS_URL":       "redis://redis:6379",
		"MASTER_KEY":      "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=", // 32 bytes
		"SESSION_SECRET":  "a-session-secret-of-at-least-32-characters",
		"ALLOWED_ORIGINS": "https://dashboard.example.com",
		"PUBLIC_BASE_URL": "https://gateway.example.com",
		"MAIL_FROM":       "no-reply@example.com",
		"SMTP_URL":        "smtp://user:password@smtp.example.com:587",
	}
}

// apply sets every variable Load reads, so a value left over in the developer's
// own environment cannot change what a test sees. Anything the case does not
// name is explicitly cleared.
func apply(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range []string{
		"APP_ENV", "PORT", "ALLOWED_ORIGINS", "PUBLIC_BASE_URL", "TRUSTED_PROXIES",
		"DATABASE_URL", "DATABASE_URL_LOCAL", "REDIS_URL", "REDIS_URL_LOCAL",
		"MASTER_KEY", "SESSION_SECRET", "SESSION_TTL", "SESSION_IDLE_TTL",
		"COOKIE_DOMAIN", "MAIL_FROM", "SMTP_URL", "SEMANTIC_CACHE", "CONTEXT_HANDOFF",
		"VERIFICATION_TTL", "RESET_TTL", "OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		t.Setenv(key, env[key])
	}
}

func TestLoadAcceptsAProductionEnvironment(t *testing.T) {
	apply(t, productionEnv(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction() = false")
	}
	if cfg.SMTPURL == "" {
		t.Error("SMTPURL is empty")
	}
}

// The mail settings fail closed in production, each naming itself, because the
// failure they prevent is silent: mail that never arrives, or a reset link in
// a log. Load reports every problem at once, so one run proves all of them.
func TestLoadRequiresMailSettingsInProduction(t *testing.T) {
	env := productionEnv(t)
	delete(env, "SMTP_URL")
	delete(env, "MAIL_FROM")
	delete(env, "PUBLIC_BASE_URL")
	apply(t, env)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error, want a refusal")
	}
	for _, want := range []string{"SMTP_URL", "MAIL_FROM", "PUBLIC_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s:\n%v", want, err)
		}
	}
}

// Outside production the same three are optional: mail goes to the log, which
// is how a developer reads a verification link without a relay.
func TestLoadAllowsNoMailProviderOutsideProduction(t *testing.T) {
	apply(t, map[string]string{
		"APP_ENV":            "development",
		"DATABASE_URL_LOCAL": "postgres://gateway:gateway@127.0.0.1:5432/llm_gateway?sslmode=disable",
		"REDIS_URL_LOCAL":    "redis://127.0.0.1:6379",
		"MASTER_KEY":         "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=",
		"SESSION_SECRET":     "a-session-secret-of-at-least-32-characters",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if cfg.SMTPURL != "" {
		t.Errorf("SMTPURL = %q, want empty", cfg.SMTPURL)
	}
	if cfg.MailFrom != "no-reply@localhost" {
		t.Errorf("MailFrom = %q, want the default", cfg.MailFrom)
	}
}

func TestLoadRejectsBadMailSettings(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"smtp url with no scheme", "SMTP_URL", "smtp.example.com:587", "SMTP_URL"},
		{"smtp url with the wrong scheme", "SMTP_URL", "https://smtp.example.com", "SMTP_URL"},
		{"smtp url with no host", "SMTP_URL", "smtp://", "SMTP_URL"},
		{"mail from without a domain", "MAIL_FROM", "no-reply", "MAIL_FROM"},
		{"mail from with a line break", "MAIL_FROM", "a@b.com\r\nBcc: c@d.com", "MAIL_FROM"},
		{"public base url with no scheme", "PUBLIC_BASE_URL", "gateway.example.com", "PUBLIC_BASE_URL"},
		// A reset link is a bearer secret, so production will not mail one
		// over a scheme that puts it in cleartext.
		{"public base url over http in production", "PUBLIC_BASE_URL", "http://gateway.example.com", "https"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := productionEnv(t)
			env[tc.key] = tc.value
			apply(t, env)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q = nil error, want a refusal", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q:\n%v", tc.want, err)
			}
		})
	}
}

// MASTER_KEY is the gate the spec names, and it had no test.
func TestLoadRequiresAUsableMasterKey(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"absent", ""},
		{"not base64", "replace-with-32-random-bytes-in-base64"},
		{"too short", "YWJjZGVm"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := productionEnv(t)
			env["MASTER_KEY"] = tc.value
			apply(t, env)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() with MASTER_KEY=%q = nil error, want a refusal", tc.value)
			} else if !strings.Contains(err.Error(), "MASTER_KEY") {
				t.Errorf("error does not name MASTER_KEY:\n%v", err)
			}
		})
	}
}

// An empty ALLOWED_ORIGINS is not a permissive default. The same-origin guard
// refuses every state-changing request that carries an Origin, so a production
// deployment with no list answers 403 to its own dashboard -- previously with
// nothing in the loader's output to say why.
func TestLoadRequiresAllowedOriginsInProduction(t *testing.T) {
	env := productionEnv(t)
	delete(env, "ALLOWED_ORIGINS")
	apply(t, env)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "ALLOWED_ORIGINS") {
		t.Errorf("error does not name ALLOWED_ORIGINS:\n%v", err)
	}
}

// Normalization happens once, here, which is what lets the CORS headers and
// the same-origin guard agree. Both read the list through Origins.Allows.
func TestLoadNormalizesAllowedOrigins(t *testing.T) {
	env := productionEnv(t)
	env["ALLOWED_ORIGINS"] = " HTTPS://Dashboard.Example.com/ , https://admin.example.com "
	apply(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	want := Origins{"https://dashboard.example.com", "https://admin.example.com"}
	if len(cfg.AllowedOrigins) != len(want) {
		t.Fatalf("AllowedOrigins = %v, want %v", cfg.AllowedOrigins, want)
	}
	for i := range want {
		if cfg.AllowedOrigins[i] != want[i] {
			t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.AllowedOrigins[i], want[i])
		}
	}

	// A browser sends the same origin in a spelling the entry did not use.
	for _, origin := range []string{"https://dashboard.example.com", "HTTPS://DASHBOARD.EXAMPLE.COM"} {
		if !cfg.AllowedOrigins.Allows(origin) {
			t.Errorf("Allows(%q) = false, want true", origin)
		}
	}
	for _, origin := range []string{"", "https://evil.example", "https://dashboard.example.com.evil.test"} {
		if cfg.AllowedOrigins.Allows(origin) {
			t.Errorf("Allows(%q) = true, want false", origin)
		}
	}
}

// An origin is a scheme, a host and a port. Anything carrying more than that
// cannot match a browser's Origin header, so a list holding one is a list that
// silently does not cover the dashboard it names.
func TestLoadRejectsOriginsThatAreNotBareOrigins(t *testing.T) {
	for _, value := range []string{
		"https://dashboard.example.com/app",
		"https://dashboard.example.com?x=1",
		"https://dashboard.example.com#frag",
		"https://user:pass@dashboard.example.com",
		"dashboard.example.com",
	} {
		t.Run(value, func(t *testing.T) {
			env := productionEnv(t)
			env["ALLOWED_ORIGINS"] = value
			apply(t, env)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with ALLOWED_ORIGINS=%q = nil error, want a refusal", value)
			}
			if !strings.Contains(err.Error(), "ALLOWED_ORIGINS") {
				t.Errorf("error does not name ALLOWED_ORIGINS:\n%v", err)
			}
		})
	}
}

// Both mailed-token lifetimes are configurable and defaulted here, because this
// is the only place a duration is decided. They used to be reachable only
// through a default inside internal/auth, documented as policy and settable by
// nothing.
func TestLoadOwnsTheMailedTokenLifetimes(t *testing.T) {
	apply(t, productionEnv(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if cfg.VerificationTTL != 24*time.Hour {
		t.Errorf("VerificationTTL = %s, want 24h", cfg.VerificationTTL)
	}
	if cfg.ResetTTL != 15*time.Minute {
		t.Errorf("ResetTTL = %s, want 15m", cfg.ResetTTL)
	}

	env := productionEnv(t)
	env["VERIFICATION_TTL"] = "2h"
	env["RESET_TTL"] = "90s"
	apply(t, env)

	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if cfg.VerificationTTL != 2*time.Hour {
		t.Errorf("VerificationTTL = %s, want 2h", cfg.VerificationTTL)
	}
	if cfg.ResetTTL != 90*time.Second {
		t.Errorf("ResetTTL = %s, want 90s", cfg.ResetTTL)
	}

	env["RESET_TTL"] = "-5m"
	apply(t, env)
	if _, err := Load(); err == nil {
		t.Fatal("Load() with a negative RESET_TTL = nil error, want a refusal")
	}
}

func TestLoadReadsTheOTLPEndpoint(t *testing.T) {
	env := productionEnv(t)
	env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "  http://127.0.0.1:4318 "
	apply(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.OTLPEndpoint != "http://127.0.0.1:4318" {
		t.Errorf("OTLPEndpoint = %q, want the trimmed value", cfg.OTLPEndpoint)
	}
}

// Telemetry is optional even in production: an unset endpoint must not stop
// the service from starting.
func TestLoadAllowsNoOTLPEndpointInProduction(t *testing.T) {
	apply(t, productionEnv(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", cfg.OTLPEndpoint)
	}
}
