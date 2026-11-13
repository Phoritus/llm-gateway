// Shared fixture for the package's database-backed tests: the real Postgres
// and Redis, a mailer that captures messages, and an HTTP client that carries
// cookies and CSRF. It is separate so the test files split by concern can all
// use it without any one of them owning it.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/crypto"
	"llm-gateway/internal/gateway"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/router"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
	"llm-gateway/internal/usage"
)

// These tests need the real Postgres and Redis from docker-compose.yml,
// because what they check — transactional signup, the session join,
// cross-tenant filtering, the rate-limit buckets — lives in SQL and in Redis
// and would be meaningless against a fake.
//
// DATABASE_TEST_URL must point at a database whose contents can be destroyed,
// because the fixture truncates every table. REDIS_TEST_URL need not be
// expendable: the fixture deletes this package's own rl:* keys and never
// flushes, for the reason newTestEnv gives below.
const (
	envTestDatabase = "DATABASE_TEST_URL"
	envTestRedis    = "REDIS_TEST_URL"

	testPassword    = "a-perfectly-good-passphrase"
	testNewPassword = "an-entirely-different-one"
)

// captureMailer keeps sent messages so a test can read the link out of one,
// which is how the verification and reset flows are driven end to end.
type captureMailer struct {
	mu       sync.Mutex
	messages []auth.Message
}

func (m *captureMailer) Send(_ context.Context, msg auth.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)
	return nil
}

func (m *captureMailer) last(t *testing.T) auth.Message {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.messages) == 0 {
		t.Fatal("no email was sent")
	}
	return m.messages[len(m.messages)-1]
}

func (m *captureMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

var tokenPattern = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

// tokenFrom pulls the secret out of the link in an email body.
func tokenFrom(t *testing.T, msg auth.Message) string {
	t.Helper()
	match := tokenPattern.FindStringSubmatch(msg.Body)
	if len(match) != 2 {
		t.Fatalf("no token in the email body:\n%s", msg.Body)
	}
	token, err := url.QueryUnescape(match[1])
	if err != nil {
		t.Fatalf("unescape token: %v", err)
	}
	return token
}

type testEnv struct {
	t      *testing.T
	server *httptest.Server
	store  *store.Store
	mailer *captureMailer
	deps   Deps
}

// newTestEnv wires the same handler tree main.go builds, against the real
// dependencies, and leaves the databases empty.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	databaseURL := os.Getenv(envTestDatabase)
	redisURL := os.Getenv(envTestRedis)
	if databaseURL == "" || redisURL == "" {
		t.Skipf("set %s and %s to run the control plane integration tests", envTestDatabase, envTestRedis)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(st.Close)

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	truncateAll(t, st)

	rdb, err := store.OpenRedis(ctx, redisURL)
	if err != nil {
		t.Fatalf("open test Redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	// Rate-limit buckets are keyed by address, and every test calls from
	// 127.0.0.1, so leftover buckets would leak between tests.
	//
	// Only this package's own keys are dropped, never the whole database.
	// REDIS_TEST_URL names one logical database and `go test ./...` runs the
	// packages in parallel, so a FLUSHDB here lands in the middle of
	// internal/ratelimit's bucket tests and refills a bucket they are in the
	// process of draining — which showed up there as one request allowed past
	// the burst, in a package this fixture does not touch.
	deleteKeys(t, rdb, "rl:*")

	mailer := &captureMailer{}
	cfg := &config.Config{
		Env:             config.EnvDevelopment,
		AllowedOrigins:  config.Origins{"http://localhost:3000"},
		SessionTTL:      time.Hour,
		SessionIdleTTL:  time.Hour,
		VerificationTTL: time.Hour,
		ResetTTL:        15 * time.Minute,
		SessionSecret:   []byte("a session secret long enough for tests"),
		MailFrom:        "test@localhost",
	}

	// Every duration is stated. internal/auth defaults none of them, because
	// config.Load is the one place a lifetime is decided, so a zero left here
	// would be a zero at runtime.
	authSvc := auth.NewService(st, auth.Config{
		SessionTTL:      cfg.SessionTTL,
		SessionIdleTTL:  cfg.SessionIdleTTL,
		VerificationTTL: cfg.VerificationTTL,
		ResetTTL:        cfg.ResetTTL,
		Secure:          false,
		SessionSecret:   cfg.SessionSecret,
		// Stated rather than left to the zero value: with no public proxy
		// trusted, X-Forwarded-For is ignored and every test shares the
		// 127.0.0.1 bucket, which is what the bucket assertions below assume.
		TrustedProxies: nil,
	})

	keyring, err := crypto.NewKeyring([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}

	deps := Deps{
		Config:      cfg,
		Store:       st,
		Credentials: store.NewCredentials(st, keyring),
		Providers: map[provider.Vendor]provider.Provider{
			provider.VendorMock: &mock.Provider{},
			provider.VendorGroq: keyChecker{valid: apiTestSecret},
		},
		GatewayKeys: auth.NewGatewayKeys(st),
		Auth:        authSvc,
		Limiter:     ratelimit.New(rdb),
		Mailer:      mailer,
		// Handler errors are expected in several tests; keep them out of the
		// test output unless something needs debugging.
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	// The real recorder over the real store, so a /v1 request in a test shows
	// up in /api/usage the way it does in production.
	usageRecorder := usage.NewRecorder(st, 1000, deps.Logger)
	go usageRecorder.Run(context.Background())
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		usageRecorder.Close(closeCtx)
	})

	mux := http.NewServeMux()
	Health(deps.Logger,
		Check{Name: "postgres", Ping: st.Ping},
		Check{Name: "redis", Ping: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	)(mux)
	mux.Handle("/api/", http.StripPrefix("/api", Routes(deps)))
	mux.Handle("/v1/", http.StripPrefix("/v1", gateway.Routes(gateway.Deps{
		Keys:      deps.GatewayKeys,
		Routes:    deps.Credentials,
		Providers: deps.Providers,
		// In memory, so one test's failing targets are not skipped in the next.
		Router:  router.New(router.NewMemory(), deps.Logger),
		Usage:   usageRecorder,
		Timeout: time.Minute,
		Limiter: ratelimit.NewFailOpen(deps.Limiter, ratelimit.NewLocal(), deps.Logger),
		Limits: gateway.Limits{
			KeyRequests:          ratelimit.PerMinute(120),
			KeyTokens:            ratelimit.PerMinute(200_000),
			WorkspaceRequests:    ratelimit.PerMinute(300),
			WorkspaceTokens:      ratelimit.PerMinute(500_000),
			WorkspaceConcurrency: 20,
		},
		ClientKey: authSvc.RateLimitKey,
		Logger:    deps.Logger,
	})))

	server := httptest.NewServer(telemetry.RequestID(mux))
	t.Cleanup(server.Close)

	// PUBLIC_BASE_URL is what the emailed links are built from.
	cfg.PublicBaseURL = server.URL

	return &testEnv{t: t, server: server, store: st, mailer: mailer, deps: deps}
}

func truncateAll(t *testing.T, st *store.Store) {
	t.Helper()
	// One statement so the foreign keys never see an inconsistent state.
	_, err := st.Pool().Exec(context.Background(),
		`TRUNCATE route_targets, routes, gateway_keys, provider_credentials, sessions, email_verification_tokens, password_reset_tokens,
		          workspace_members, workspaces, users CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// deleteKeys removes every Redis key matching a pattern, scanning rather than
// using KEYS so the call stays incremental.
func deleteKeys(t *testing.T, rdb *redis.Client, pattern string) {
	t.Helper()
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, pattern, 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("scan %s: %v", pattern, err)
	}
	if len(keys) == 0 {
		return
	}
	if err := rdb.Del(ctx, keys...).Err(); err != nil {
		t.Fatalf("delete %s: %v", pattern, err)
	}
}

// client is one browser: a cookie jar, plus the CSRF header handling a real
// dashboard would do.
type client struct {
	t      *testing.T
	env    *testEnv
	http   *http.Client
	noCSRF bool
	origin string
	// forwardedFor, when set, is sent as X-Forwarded-For. It stands in for a
	// client claiming to be somewhere else.
	forwardedFor string
}

func (e *testEnv) newClient() *client {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatalf("cookie jar: %v", err)
	}
	return &client{
		t:   e.t,
		env: e,
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			// Follow nothing: every endpoint here answers with JSON, and a
			// redirect would hide a wrong status.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		origin: "http://localhost:3000",
	}
}

type response struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

// errorCode returns the OpenAI-shaped error code, or "" when the body is not
// an error.
func (r response) errorCode() string {
	errObj, ok := r.body["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}

func (c *client) do(method, path string, body any) response {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.env.server.URL+path, reader)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", c.origin)
	if c.forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", c.forwardedFor)
	}
	if !c.noCSRF {
		if token := c.csrfToken(); token != "" {
			req.Header.Set(auth.CSRFHeader, token)
		}
	}

	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}

	out := response{status: res.StatusCode, raw: string(raw), header: res.Header}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			c.t.Fatalf("%s %s returned a body that is not JSON: %q", method, path, raw)
		}
	}
	return out
}

// csrfToken reads the readable CSRF cookie, as the dashboard's JavaScript
// would.
func (c *client) csrfToken() string {
	u, err := url.Parse(c.env.server.URL)
	if err != nil {
		c.t.Fatalf("parse server URL: %v", err)
	}
	for _, cookie := range c.http.Jar.Cookies(u) {
		if cookie.Name == c.env.deps.Auth.CSRFCookieName() {
			return cookie.Value
		}
	}
	return ""
}

func (c *client) sessionCookie() string {
	u, _ := url.Parse(c.env.server.URL)
	for _, cookie := range c.http.Jar.Cookies(u) {
		if cookie.Name == c.env.deps.Auth.SessionCookieName() {
			return cookie.Value
		}
	}
	return ""
}

// signup registers an account and returns the verification token from the
// email that was sent.
func (e *testEnv) signup(c *client, email string) string {
	e.t.Helper()
	res := c.do(http.MethodPost, "/api/auth/signup", map[string]any{
		"email":    email,
		"password": testPassword,
	})
	if res.status != http.StatusCreated {
		e.t.Fatalf("signup %s: status = %d, body = %s", email, res.status, res.raw)
	}
	return tokenFrom(e.t, e.mailer.last(e.t))
}

func (e *testEnv) login(c *client, email, password string) response {
	e.t.Helper()
	return c.do(http.MethodPost, "/api/auth/login", map[string]any{
		"email":    email,
		"password": password,
	})
}

// signupAndLogin takes an account all the way to a usable session.
func (e *testEnv) signupAndLogin(email string) *client {
	e.t.Helper()
	c := e.newClient()

	token := e.signup(c, email)
	if res := c.do(http.MethodPost, "/api/auth/verify-email", map[string]any{"token": token}); res.status != http.StatusOK {
		e.t.Fatalf("verify %s: status = %d, body = %s", email, res.status, res.raw)
	}
	if res := e.login(c, email, testPassword); res.status != http.StatusOK {
		e.t.Fatalf("login %s: status = %d, body = %s", email, res.status, res.raw)
	}
	return c
}

func (e *testEnv) userID(email string) string {
	e.t.Helper()
	user, err := e.store.UserByEmail(context.Background(), email)
	if err != nil {
		e.t.Fatalf("look up %s: %v", email, err)
	}
	return user.ID
}

// workspaceOf returns the workspace a user owns.
func (e *testEnv) workspaceOf(email string) string {
	e.t.Helper()
	workspace, err := e.store.DefaultWorkspace(context.Background(), e.userID(email))
	if err != nil {
		e.t.Fatalf("default workspace for %s: %v", email, err)
	}
	return workspace.ID
}

// sessionFor mints a session for a user against a specific workspace and
// returns a client holding it. It stands in for the workspace switcher the
// dashboard will have, and lets a test hold a session whose workspace is not
// the user's own -- which must not authenticate.
func (e *testEnv) sessionFor(email, workspaceID string) *client {
	e.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	token, _, err := e.deps.Auth.CreateSession(context.Background(), e.userID(email), workspaceID, req)
	if err != nil {
		e.t.Fatalf("create session for %s: %v", email, err)
	}

	c := e.newClient()
	u, _ := url.Parse(e.server.URL)
	c.http.Jar.SetCookies(u, []*http.Cookie{
		{Name: e.deps.Auth.SessionCookieName(), Value: token, Path: "/"},
		{Name: e.deps.Auth.CSRFCookieName(), Value: e.deps.Auth.CSRFToken(token), Path: "/"},
	})
	return c
}
