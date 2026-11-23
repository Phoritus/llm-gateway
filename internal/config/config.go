package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Environment names the deployment this process is running in. Only
// EnvProduction tightens cookie flags; everything else is treated as
// development so that plain http://localhost keeps working.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// masterKeyLen is the AES-256 key size. The spec requires the provider
// credential master key to be exactly this long once decoded.
const masterKeyLen = 32

// minSessionSecretLen is the shortest session secret we accept. Shorter
// secrets make the CSRF token HMAC cheap to brute force.
const minSessionSecretLen = 32

// Origins is the set of browser origins allowed to make state-changing
// requests. Entries are normalized once, by Load, so the two checks that read
// this list -- the CORS headers and the same-origin guard -- cannot disagree
// about what counts as a match. They did: each kept its own copy of the
// lowercase-and-trim-slash rule, and only the loader's copy rejected an origin
// carrying a path.
type Origins []string

// Allows reports whether an Origin header names an allowed origin. An empty
// origin is never allowed; a caller that means to let a request with no Origin
// header through says so itself.
func (o Origins) Allows(origin string) bool {
	return origin != "" && slices.Contains(o, normalizeOrigin(origin))
}

// normalizeOrigin is the single definition of how two spellings of one origin
// are compared. Browsers send the scheme and host lowercased and without a
// trailing slash; a hand-written ALLOWED_ORIGINS entry often is not.
func normalizeOrigin(raw string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
}

// minShutdownTimeout leaves requests a second to finish once the time kept
// back for the flushes that follow them is taken out.
const minShutdownTimeout = 5 * time.Second

// Config is the fully validated configuration. Every field is resolved at
// startup; nothing reads os.Getenv after Load returns.
type Config struct {
	Env            Environment
	Port           int
	AllowedOrigins Origins
	DatabaseURL    string
	RedisURL       string
	// VectorRedisURL names the Redis 8 that holds the semantic cache's
	// vectors. Set only when SemanticCache is on: Upstash has no vector sets.
	VectorRedisURL string
	MasterKey      []byte
	SessionSecret  []byte
	SessionTTL     time.Duration
	SessionIdleTTL time.Duration
	// VerificationTTL and ResetTTL bound the two email-delivered tokens. A
	// reset token is short-lived because it is a password bypass; a
	// verification token is only a proof of address.
	VerificationTTL time.Duration
	ResetTTL        time.Duration
	// V1RequestTimeout is the deadline on one /v1 request, streaming included.
	// It bounds what an abandoned or stuck generation can cost.
	V1RequestTimeout time.Duration
	// The /v1 limits, per minute: requests and estimated tokens, for one
	// gateway key and for the whole workspace. The key's are the smaller so
	// that one key cannot spend the workspace's entire allowance.
	V1KeyRPM       int
	V1KeyTPM       int
	V1WorkspaceRPM int
	V1WorkspaceTPM int
	// V1WorkspaceConcurrency caps a workspace's requests in flight on one
	// instance. Long streams are what exhaust memory first.
	V1WorkspaceConcurrency int
	// UsageQueueSize is how many usage events wait to be written. A full queue
	// drops its oldest, so this bounds memory while the database is slow.
	UsageQueueSize int
	// UsageRetention is how long raw usage events are kept. The daily rollup
	// is kept for good.
	UsageRetention time.Duration
	// CacheTTL is how long the exact cache keeps an answer. Zero turns the
	// exact cache off.
	CacheTTL       time.Duration
	CookieDomain   string
	TrustedProxies []netip.Prefix
	MailFrom       string
	PublicBaseURL  string
	// SMTPURL is the relay verification and reset mail goes through, as
	// smtp://user:pass@host:587 or smtps://user:pass@host:465. Empty is
	// allowed only outside production, where mail goes to the log instead.
	SMTPURL string
	// OTLPEndpoint is where telemetry is exported. Empty disables it. The
	// credentials in OTEL_EXPORTER_OTLP_HEADERS are deliberately not read
	// here: the SDK reads them itself, so they never pass through anything
	// that could log them.
	OTLPEndpoint   string
	SemanticCache  bool
	ContextHandoff bool
	// ShutdownTimeout is the whole of a graceful shutdown, from the signal to
	// exit: draining requests, then flushing usage and telemetry. The platform
	// kills the process on a timer of its own, so this has to end first.
	ShutdownTimeout time.Duration
}

// IsProduction reports whether cookies must carry the Secure attribute and
// the __Host- prefix.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads and validates the environment. The returned error names every
// problem found rather than only the first, so a misconfigured deployment can
// be fixed in one pass.
func Load() (*Config, error) {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// Read before defaulting, because production requires these to have been
	// set rather than merely to have a value.
	mailFrom := strings.TrimSpace(os.Getenv("MAIL_FROM"))
	publicBaseURL := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL"))

	cfg := &Config{
		Env:           Environment(strings.ToLower(envOr("APP_ENV", string(EnvDevelopment)))),
		CookieDomain:  os.Getenv("COOKIE_DOMAIN"),
		MailFrom:      envOr("MAIL_FROM", "no-reply@localhost"),
		PublicBaseURL: strings.TrimRight(envOr("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		SMTPURL:       strings.TrimSpace(os.Getenv("SMTP_URL")),
		OTLPEndpoint:  os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}

	if cfg.Env != EnvDevelopment && cfg.Env != EnvProduction {
		fail("APP_ENV must be %q or %q, got %q", EnvDevelopment, EnvProduction, cfg.Env)
	}

	port, err := strconv.Atoi(envOr("PORT", "8080"))
	switch {
	case err != nil:
		fail("PORT must be a number: %v", err)
	case port < 1 || port > 65535:
		fail("PORT must be between 1 and 65535, got %d", port)
	default:
		cfg.Port = port
	}

	var name string
	if name, cfg.DatabaseURL = connectionURL(cfg.Env, "DATABASE_URL"); cfg.DatabaseURL == "" {
		fail("%s is required when APP_ENV is %q", name, cfg.Env)
	}
	if name, cfg.RedisURL = connectionURL(cfg.Env, "REDIS_URL"); cfg.RedisURL == "" {
		fail("%s is required when APP_ENV is %q", name, cfg.Env)
	}

	// The spec is explicit: the service must refuse to start without a usable
	// master key, because provider credentials are unreadable without it and a
	// silently generated one would decrypt nothing.
	switch raw := os.Getenv("MASTER_KEY"); {
	case raw == "":
		fail("MASTER_KEY is required")
	default:
		key, err := decodeKey(raw)
		if err != nil {
			fail("MASTER_KEY must be base64: %v", err)
		} else if len(key) != masterKeyLen {
			fail("MASTER_KEY must decode to %d bytes, got %d", masterKeyLen, len(key))
		} else {
			cfg.MasterKey = key
		}
	}

	switch secret := os.Getenv("SESSION_SECRET"); {
	case secret == "":
		fail("SESSION_SECRET is required")
	case len(secret) < minSessionSecretLen:
		fail("SESSION_SECRET must be at least %d characters, got %d", minSessionSecretLen, len(secret))
	default:
		cfg.SessionSecret = []byte(secret)
	}

	for _, entry := range splitList(os.Getenv("ALLOWED_ORIGINS")) {
		origin := normalizeOrigin(entry)
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			// A rejected entry is dropped rather than kept: Load returns an
			// error anyway, and a half-validated list must not be what a future
			// caller reads if that stops being true.
			fail("ALLOWED_ORIGINS entry %q must be a bare scheme://host[:port]", entry)
			continue
		}
		cfg.AllowedOrigins = append(cfg.AllowedOrigins, origin)
	}
	// An empty list is not a permissive default: the same-origin guard refuses
	// every state-changing request carrying an Origin, so the control plane
	// answers 403 to its own dashboard while the loader says nothing. In
	// development that is survivable, because a request with no Origin header
	// at all -- curl, the test client -- still passes.
	if len(cfg.AllowedOrigins) == 0 && cfg.Env == EnvProduction {
		fail("ALLOWED_ORIGINS is required when APP_ENV is %q, because an empty list refuses every browser request that changes state", EnvProduction)
	}

	// Rate-limit buckets are keyed on the client address, so whether
	// X-Forwarded-For may be believed is a security decision and not a
	// convenience. The private ranges are always treated as proxies by
	// internal/auth -- loopback deliberately is not -- and this list adds the
	// public ranges of whatever CDN sits in front; empty means the header is
	// ignored entirely.
	for _, entry := range splitList(os.Getenv("TRUSTED_PROXIES")) {
		prefix, err := parsePrefix(entry)
		if err != nil {
			fail("TRUSTED_PROXIES entry %q must be an IP or CIDR range: %v", entry, err)
			continue
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, prefix)
	}

	// An absolute session lifetime and a shorter idle window are both required
	// by the session design; an idle window longer than the absolute one would
	// never trigger, so that combination is a configuration error.
	if d, err := durationOr("SESSION_TTL", 14*24*time.Hour); err != nil {
		fail("%v", err)
	} else {
		cfg.SessionTTL = d
	}
	if d, err := durationOr("SESSION_IDLE_TTL", 24*time.Hour); err != nil {
		fail("%v", err)
	} else {
		cfg.SessionIdleTTL = d
	}
	if cfg.SessionTTL > 0 && cfg.SessionIdleTTL > cfg.SessionTTL {
		fail("SESSION_IDLE_TTL (%s) must not exceed SESSION_TTL (%s)", cfg.SessionIdleTTL, cfg.SessionTTL)
	}

	// The two mailed tokens are bounded here too, so that every duration the
	// service runs on is decided in this function. They were previously
	// reachable only through a default inside internal/auth: documented as
	// policy, and unconfigurable.
	if d, err := durationOr("VERIFICATION_TTL", 24*time.Hour); err != nil {
		fail("%v", err)
	} else {
		cfg.VerificationTTL = d
	}
	if d, err := durationOr("RESET_TTL", 15*time.Minute); err != nil {
		fail("%v", err)
	} else {
		cfg.ResetTTL = d
	}
	if d, err := durationOr("V1_REQUEST_TIMEOUT", 5*time.Minute); err != nil {
		fail("%v", err)
	} else {
		cfg.V1RequestTimeout = d
	}
	if d, err := durationOr("SHUTDOWN_TIMEOUT", 25*time.Second); err != nil {
		fail("%v", err)
	} else if d < minShutdownTimeout {
		fail("SHUTDOWN_TIMEOUT must be at least %s, got %s", minShutdownTimeout, d)
	} else {
		cfg.ShutdownTimeout = d
	}
	for _, limit := range []struct {
		key      string
		fallback int
		field    *int
	}{
		{"V1_KEY_RPM", 120, &cfg.V1KeyRPM},
		{"V1_KEY_TPM", 200_000, &cfg.V1KeyTPM},
		{"V1_WORKSPACE_RPM", 300, &cfg.V1WorkspaceRPM},
		{"V1_WORKSPACE_TPM", 500_000, &cfg.V1WorkspaceTPM},
		{"V1_WORKSPACE_CONCURRENCY", 20, &cfg.V1WorkspaceConcurrency},
	} {
		if n, err := positiveIntOr(limit.key, limit.fallback); err != nil {
			fail("%v", err)
		} else {
			*limit.field = n
		}
	}

	if n, err := positiveIntOr("USAGE_QUEUE_SIZE", 10_000); err != nil {
		fail("%v", err)
	} else {
		cfg.UsageQueueSize = n
	}
	if d, err := durationOr("USAGE_RETENTION", 30*24*time.Hour); err != nil {
		fail("%v", err)
	} else {
		cfg.UsageRetention = d
	}
	if d, err := durationOrOff("CACHE_TTL", time.Hour); err != nil {
		fail("%v", err)
	} else {
		cfg.CacheTTL = d
	}

	// Mail is load-bearing, not decorative: an account cannot be used until its
	// address is confirmed, so a deployment that cannot send is a deployment
	// nobody can sign up for. All three of these fail closed in production for
	// the same reason MASTER_KEY does -- the broken version is silent.
	if cfg.SMTPURL == "" {
		if cfg.Env == EnvProduction {
			// Without a relay the only mailer is the one that writes the body
			// to the log, and the body contains a password-reset link: a
			// 15-minute account takeover for anyone who can read the logs.
			fail("SMTP_URL is required when APP_ENV is %q, because the fallback writes reset links to the log", EnvProduction)
		}
	} else if u, err := url.Parse(cfg.SMTPURL); err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" {
		fail("SMTP_URL must look like smtp://user:password@host:587 or smtps://user:password@host:465")
	}

	if mailFrom == "" && cfg.Env == EnvProduction {
		// The no-reply@localhost default is refused by every real relay, and
		// the refusal would surface as mail that silently never arrives.
		fail("MAIL_FROM is required when APP_ENV is %q", EnvProduction)
	} else if !strings.Contains(cfg.MailFrom, "@") || strings.ContainsAny(cfg.MailFrom, "\r\n ") {
		fail("MAIL_FROM must be a single email address, got %q", cfg.MailFrom)
	}

	// PUBLIC_BASE_URL is the host part of every emailed link. Left at its
	// default in production it sends links pointing at the recipient's own
	// machine, which fails in a way that looks like the mail never arrived.
	if publicBaseURL == "" && cfg.Env == EnvProduction {
		fail("PUBLIC_BASE_URL is required when APP_ENV is %q, because it is where verification and reset links point", EnvProduction)
	} else if u, err := url.Parse(cfg.PublicBaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		fail("PUBLIC_BASE_URL must be an absolute http or https URL, got %q", cfg.PublicBaseURL)
	} else if cfg.Env == EnvProduction && u.Scheme != "https" {
		// A reset link is a bearer secret; over http it is readable by anyone
		// on the path, which defeats the point of mailing it rather than
		// showing it.
		fail("PUBLIC_BASE_URL must use https when APP_ENV is %q, got %q", EnvProduction, cfg.PublicBaseURL)
	}

	// The SDK reads this variable again by itself, untrimmed, and on a value it
	// cannot parse it logs once and exports to https://localhost:4318 instead.
	// That surfaces later as unrelated-looking TLS errors, so the exact value
	// the SDK will see is checked here.
	if cfg.OTLPEndpoint != "" {
		if u, err := url.Parse(cfg.OTLPEndpoint); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			strings.ContainsAny(cfg.OTLPEndpoint, " \t\r\n\"'") {
			fail("OTEL_EXPORTER_OTLP_ENDPOINT must be an absolute http or https URL, got %q", cfg.OTLPEndpoint)
		}
	}

	if b, err := boolOr("SEMANTIC_CACHE", false); err != nil {
		fail("%v", err)
	} else {
		cfg.SemanticCache = b
	}
	if cfg.SemanticCache {
		if name, cfg.VectorRedisURL = connectionURL(cfg.Env, "VECTOR_REDIS_URL"); cfg.VectorRedisURL == "" {
			fail("%s is required when SEMANTIC_CACHE is on", name)
		}
	}
	if b, err := boolOr("CONTEXT_HANDOFF", false); err != nil {
		fail("%v", err)
	} else {
		cfg.ContextHandoff = b
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// decodeKey accepts both standard and URL base64, with or without padding,
// because key material gets copied between tools that disagree on the dialect.
func decodeKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		if key, err := enc.DecodeString(raw); err == nil {
			return key, nil
		}
	}
	return nil, errors.New("not valid base64")
}

// parsePrefix accepts a CIDR range or a single address, so a one-host proxy
// can be written as 203.0.113.7 rather than 203.0.113.7/32.
func parsePrefix(raw string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, errors.New("not an IP address or CIDR range")
	}
	addr = addr.Unmap().WithZone("")
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// connectionURL reads a dependency's connection string from the variable that
// belongs to the current environment, and reports which name it read so an
// error can name it too. Production reads DATABASE_URL; anything else reads
// DATABASE_URL_LOCAL.
//
// Neither name falls back to the other, which is the whole point. One name for
// both meant the local and the deployed URL competed for the same line of
// .env: a second DATABASE_URL further down the file wins silently, and a
// service started for local development then migrates and writes to the
// production database with nothing in its output to say which one it opened.
// Separate names make that mistake impossible to make quietly — pointing
// development at a remote database is still allowed, but only by writing that
// URL into DATABASE_URL_LOCAL, which says what it is.
func connectionURL(env Environment, name string) (string, string) {
	if env != EnvProduction {
		name += "_LOCAL"
	}
	return name, os.Getenv(name)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// splitList splits a variable separated by commas, whitespace or both into its
// entries. It does no further cleaning: what counts as normalized differs per
// setting, and an origin's rule has no business being applied to a CIDR range.
func splitList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 24h: %v", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}

// durationOrOff is durationOr for a setting that zero switches off.
func durationOrOff(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 1h, or 0 for off: %v", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %s", key, d)
	}
	return d, nil
}

func positiveIntOr(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number: %v", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", key, n)
	}
	return n, nil
}

// boolOr reads the on/off spelling the .env.example file uses alongside the
// usual true/false, so the documented values work as written.
func boolOr(key string, fallback bool) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch raw {
	case "":
		return fallback, nil
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be on or off, got %q", key, raw)
	}
}
