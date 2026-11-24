// Command gateway runs the BYOK LLM gateway: the control plane under /api and
// the OpenAI-compatible data plane under /v1.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"llm-gateway/internal/api"
	"llm-gateway/internal/auth"
	"llm-gateway/internal/cache"
	"llm-gateway/internal/config"
	"llm-gateway/internal/crypto"
	"llm-gateway/internal/gateway"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/anthropic"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/provider/openai"
	"llm-gateway/internal/quota"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/router"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
	"llm-gateway/internal/usage"
)

const (
	// shutdownReserve is the end of SHUTDOWN_TIMEOUT kept back from draining
	// requests, for what has to run after them: the router's last writes, the
	// usage flush and the telemetry flush.
	shutdownReserve = 4 * time.Second
	deregisterDelay = 3 * time.Second
	// telemetryReserve is the end of shutdownReserve the usage flush leaves
	// for the telemetry flush.
	telemetryReserve = time.Second
	// telemetryFlushTimeout bounds the telemetry flush on a return that never
	// started a shutdown, such as a failed startup.
	telemetryFlushTimeout = 5 * time.Second
	// retentionInterval is how often old raw usage events are pruned. Nothing
	// depends on the exact cadence, so it is not configuration.
	retentionInterval = time.Hour
	// catalogInterval is how often the free quota catalog is read again, which
	// is how long a corrected row takes to apply.
	catalogInterval = 10 * time.Minute
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	command := run
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "rotate-keys":
			command = rotateKeys
		default:
			logger.Error("unknown command; the only one is rotate-keys", slog.String("command", os.Args[1]))
			os.Exit(2)
		}
	}
	if err := command(logger); err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

const resealBatch = 100

func rotateKeys(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	keyring, err := crypto.NewKeyring(cfg.MasterKey, cfg.PreviousMasterKeys...)
	if err != nil {
		return err
	}
	creds := store.NewCredentials(st, keyring)
	resealed, err := creds.Reseal(ctx, resealBatch)
	logger.Info("re-sealed provider credentials", slog.Int("count", resealed), slog.String("current_key", string(keyring.Current())))
	if err != nil {
		return err
	}
	usage, err := creds.KeyUsage(ctx)
	if err != nil {
		return err
	}
	for id, n := range usage {
		logger.Info("credentials sealed under key", slog.String("key_id", string(id)), slog.Int("count", n),
			slog.Bool("current", id == keyring.Current()))
	}
	return nil
}

// shutdownPlan is when each step of a graceful shutdown gives up. The steps
// share one budget because the platform's kill timer does not wait for them
// one by one.
type shutdownPlan struct {
	deregisterBy time.Time
	// drainBy is when requests still in flight are cut.
	drainBy time.Time
	// flushBy is when the usage flush stops, leaving the rest to telemetry.
	flushBy time.Time
	stopBy  time.Time
}

func planShutdown(signalled time.Time, budget time.Duration) shutdownPlan {
	stopBy := signalled.Add(budget)
	drainBy := stopBy.Add(-shutdownReserve)
	return shutdownPlan{
		deregisterBy: signalled.Add(min(deregisterDelay, drainBy.Sub(signalled)/2)),
		drainBy:      drainBy,
		flushBy:      stopBy.Add(-telemetryReserve),
		stopBy:       stopBy,
	}
}

func run(logger *slog.Logger) error {
	// Signals are caught before anything is opened, so a Ctrl-C during
	// startup cancels the whole sequence instead of being ignored.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("configuration loaded",
		slog.String("env", string(cfg.Env)),
		slog.Int("port", cfg.Port),
		slog.Bool("semantic_cache", cfg.SemanticCache),
		slog.Bool("context_handoff", cfg.ContextHandoff),
	)

	// A deployment without an endpoint runs fine, just unobserved. That is a
	// state worth saying out loud in production and a non-event locally.
	if cfg.OTLPEndpoint == "" && cfg.IsProduction() {
		logger.Warn("OTEL_EXPORTER_OTLP_ENDPOINT is empty: metrics and traces are not being exported")
	}
	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Settings{
		Endpoint:    cfg.OTLPEndpoint,
		Environment: string(cfg.Env),
	})
	if err != nil {
		return err
	}
	// Deferred first so it runs last: after the server has drained, the last
	// requests' spans and metrics are in what gets flushed, and an early
	// return still flushes whatever was recorded before it.
	var plan shutdownPlan
	defer func() {
		deadline := plan.stopBy
		if deadline.IsZero() {
			deadline = time.Now().Add(telemetryFlushTimeout)
		}
		flushCtx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if err := shutdownTelemetry(flushCtx); err != nil {
			logger.Error("telemetry shutdown failed", slog.String("error", err.Error()))
		}
	}()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	rdb, err := store.OpenRedis(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	logger.Info("schema is up to date")

	keyring, err := crypto.NewKeyring(cfg.MasterKey, cfg.PreviousMasterKeys...)
	if err != nil {
		return err
	}

	authSvc := auth.NewService(st, auth.Config{
		SessionTTL:      cfg.SessionTTL,
		SessionIdleTTL:  cfg.SessionIdleTTL,
		VerificationTTL: cfg.VerificationTTL,
		ResetTTL:        cfg.ResetTTL,
		Secure:          cfg.IsProduction(),
		CookieDomain:    cfg.CookieDomain,
		SessionSecret:   cfg.SessionSecret,
		TrustedProxies:  cfg.TrustedProxies,
	})

	// Without the CDN's ranges, a request forwarded by it resolves to the edge
	// address that relayed it, so everyone behind one edge node shares a
	// rate-limit bucket. That errs towards refusing rather than towards a
	// bypass, which is why it is a warning and not a refusal to start.
	if cfg.IsProduction() && len(cfg.TrustedProxies) == 0 {
		logger.Warn("TRUSTED_PROXIES is empty: X-Forwarded-For from a public proxy will not be believed, " +
			"so clients behind a CDN will be rate limited per edge node rather than per client")
	}

	// With a relay configured, mail is sent. Without one it goes to the log,
	// which is a development convenience and nothing else: the body carries a
	// reset link, so config.Load refuses to start in production without
	// SMTP_URL and this branch is unreachable there.
	var mailer auth.Mailer
	if cfg.SMTPURL != "" {
		smtpMailer, err := auth.NewSMTPMailer(cfg.SMTPURL, cfg.MailFrom)
		if err != nil {
			return err
		}
		mailer = smtpMailer
		// Neither the relay host nor the sender address is logged: both name the
		// operator's mail provider and domain to anyone who can read the logs.
		logger.Info("mail delivery over smtp")
	} else {
		mailer = auth.LogMailer{Logger: logger, From: cfg.MailFrom}
		logger.Warn("no SMTP_URL: verification and reset links will be written to the log, " +
			"which is development only -- production refuses to start without one")
	}

	gatewayKeys := auth.NewGatewayKeys(st)

	providers := map[provider.Vendor]provider.Provider{}
	for vendor, endpoint := range openai.Endpoints {
		providers[vendor] = openai.New(endpoint, openai.Options{})
	}
	providers[provider.VendorCustom] = openai.NewCustom(openai.Options{})
	providers[provider.VendorAnthropic] = anthropic.New(anthropic.Options{})
	if !cfg.IsProduction() {
		providers[provider.VendorMock] = &mock.Provider{}
	}

	limiter := ratelimit.New(rdb)

	deps := api.Deps{
		Config:      cfg,
		Store:       st,
		Credentials: store.NewCredentials(st, keyring),
		Providers:   providers,
		GatewayKeys: gatewayKeys,
		Auth:        authSvc,
		Limiter:     limiter,
		Mailer:      mailer,
		Logger:      logger,
	}

	mux := http.NewServeMux()
	// Readiness reports on what this process needs to serve a request, and the
	// list is written here because this is where those clients are opened.
	// Redis counts: the control plane's rate limiting refuses requests when it
	// is unavailable, so an instance without it cannot serve a login. /v1 keeps
	// answering through its in-process limiter until the instance is dropped.
	var draining atomic.Bool
	checks := []api.Check{
		{Name: "postgres", Ping: st.Ping},
		{Name: "redis", Ping: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	}
	api.Health(logger, draining.Load, checks...)(mux)
	if err := st.RegisterMetrics(); err != nil {
		return err
	}
	for _, check := range checks {
		if err := telemetry.ObserveUp(check.Name, check.Ping); err != nil {
			return err
		}
	}
	mux.Handle("/api/", http.StripPrefix("/api", api.Routes(deps)))

	fallback := router.New(router.NewRedis(rdb), logger)
	usageRecorder := usage.NewRecorder(st, cfg.UsageQueueSize, logger)
	if err := usageRecorder.RegisterMetrics(); err != nil {
		return err
	}
	v1Limiter := ratelimit.NewFailOpen(limiter, ratelimit.NewLocal(), logger)

	// A catalog that cannot be read leaves every route in its own order, which
	// is the service working without one of its optimizations, so it is not a
	// reason to refuse to start. The refresh job tries again.
	catalog := &quota.Catalog{}
	if err := catalog.Load(ctx, st); err != nil {
		logger.Warn("free quota catalog not loaded: free-first routes keep their own order until it is",
			slog.String("error", err.Error()))
	}

	// Assigned only when the cache is on: a nil *cache.Exact inside the
	// interface would not compare equal to nil in the handler.
	var responseCache gateway.ResponseCache
	if cfg.CacheTTL > 0 {
		responseCache = cache.NewExact(rdb, cfg.CacheTTL)
	} else {
		logger.Info("CACHE_TTL is 0: the exact cache is off")
	}

	var (
		semanticCache gateway.SemanticCache
		embeddings    *cache.Embeddings
	)
	if cfg.SemanticCache {
		vectors, err := newVectorRedis(ctx, cfg.VectorRedisURL, logger)
		if err != nil {
			return err
		}
		defer func() { _ = vectors.Close() }()
		semanticCache = cache.NewSemantic(vectors)
		embeddings = cache.NewEmbeddings(embedders(providers), semanticEmbedTimeout)
	}

	mux.Handle("/v1/", http.StripPrefix("/v1", gateway.Routes(gateway.Deps{
		Keys:           gatewayKeys,
		Routes:         deps.Credentials,
		Aliases:        st,
		Providers:      providers,
		Router:         fallback,
		Usage:          usageRecorder,
		Timeout:        cfg.V1RequestTimeout,
		Cache:          responseCache,
		Semantic:       semanticCache,
		Embeddings:     embeddings,
		Quota:          quota.New(catalog, v1Limiter, logger),
		ContextHandoff: cfg.ContextHandoff,
		Limiter:        v1Limiter,
		Limits: gateway.Limits{
			KeyRequests:          ratelimit.PerMinute(float64(cfg.V1KeyRPM)),
			KeyTokens:            ratelimit.PerMinute(float64(cfg.V1KeyTPM)),
			WorkspaceRequests:    ratelimit.PerMinute(float64(cfg.V1WorkspaceRPM)),
			WorkspaceTokens:      ratelimit.PerMinute(float64(cfg.V1WorkspaceTPM)),
			WorkspaceConcurrency: cfg.V1WorkspaceConcurrency,
		},
		ClientKey: authSvc.RateLimitKey,
		Logger:    logger,
	})))

	// otelhttp is outermost so the span exists when the access log reads its
	// trace id. Route sits next to the mux because it needs the request the
	// mux matched.
	handler := otelhttp.NewHandler(
		telemetry.RequestID(telemetry.AccessLog(logger)(telemetry.Route(mux))),
		"http.server",
	)

	srv := &http.Server{
		Addr:    net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler: handler,
		// A slow or absent header set must not hold a connection open
		// indefinitely; this is the one timeout that is safe to set globally,
		// since a streaming /v1 response needs no write deadline.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	sweeperCtx, stopSweeper := context.WithCancel(context.Background())
	flusherDone := make(chan struct{})
	// Waits for the flusher's last write on every return path, before the
	// deferred pool close.
	defer func() {
		stopSweeper()
		<-flusherDone
	}()
	go authSvc.RunSweeper(sweeperCtx, 0, logger)
	go usageRecorder.Run(sweeperCtx)
	go usage.RunRetention(sweeperCtx, st, cfg.UsageRetention, retentionInterval, logger)
	go catalog.Run(sweeperCtx, st, catalogInterval, logger)
	go func() {
		defer close(flusherDone)
		gatewayKeys.RunLastUsedFlusher(sweeperCtx, logger)
	}()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// SHUTDOWN_TIMEOUT covers everything from here to exit, because the
	// platform's kill does: Render sends SIGKILL 30s after SIGTERM unless
	// maxShutdownDelaySeconds raises it, to 300s at most. A step that ran past
	// the budget would be killed part-way, so each one gets a deadline inside it.
	plan = planShutdown(time.Now(), cfg.ShutdownTimeout)
	draining.Store(true)
	time.Sleep(time.Until(plan.deregisterBy))

	// A request still running at drainBy is cut. With the defaults that is any
	// stream older than about 20s, far short of V1_REQUEST_TIMEOUT: streams
	// outlive a deploy only when the platform's delay and SHUTDOWN_TIMEOUT are
	// raised together. Closing the connections cancels those requests, which
	// stops their upstream calls.
	drainCtx, cancel := context.WithDeadline(context.Background(), plan.drainBy)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		logger.Error("drain window ended with requests in flight; cutting them", slog.String("error", err.Error()))
		_ = srv.Close()
	}
	// The last failures' skips are still being written, and the deferred
	// Redis close would cut them off.
	fallback.Wait()
	// Last among the writers, while the pool is still open. A request that was
	// cut above may record after this; its event is counted as dropped for
	// shutdown.
	flushCtx, cancelFlush := context.WithDeadline(context.Background(), plan.flushBy)
	defer cancelFlush()
	usageRecorder.Close(flushCtx)
	logger.Info("stopped")
	return nil
}

const semanticEmbedTimeout = 2 * time.Second

func newVectorRedis(ctx context.Context, url string, logger *slog.Logger) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse VECTOR_REDIS_URL: %w", err)
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		logger.Warn("the vector Redis does not answer: semantic lookups miss until it does",
			slog.String("error", err.Error()))
	}
	return client, nil
}

func embedders(providers map[provider.Vendor]provider.Provider) map[provider.Vendor]provider.Embedder {
	out := map[provider.Vendor]provider.Embedder{}
	for vendor, p := range providers {
		if e, ok := p.(provider.Embedder); ok {
			out[vendor] = e
		}
	}
	return out
}
