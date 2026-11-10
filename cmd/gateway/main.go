// Command gateway runs the BYOK LLM gateway: the control plane under /api and
// (later) the OpenAI-compatible data plane under /v1.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"llm-gateway/internal/api"
	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("startup failed", slog.String("error", err.Error()))
		os.Exit(1)
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
		logger.Info("mail delivery over smtp",
			slog.String("host", smtpMailer.Host()),
			slog.String("from", cfg.MailFrom),
		)
	} else {
		mailer = auth.LogMailer{Logger: logger, From: cfg.MailFrom}
		logger.Warn("no SMTP_URL: verification and reset links will be written to the log, " +
			"which is development only -- production refuses to start without one")
	}

	deps := api.Deps{
		Config:  cfg,
		Store:   st,
		Auth:    authSvc,
		Limiter: ratelimit.New(rdb),
		Mailer:  mailer,
		Logger:  logger,
	}

	mux := http.NewServeMux()
	// Readiness reports on what this process needs to serve a request, and the
	// list is written here because this is where those clients are opened.
	// Redis counts: rate limiting refuses requests when it is unavailable, so an
	// instance without it cannot serve a login.
	api.Health(logger,
		api.Check{Name: "postgres", Ping: st.Ping},
		api.Check{Name: "redis", Ping: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	)(mux)
	mux.Handle("/api/", http.StripPrefix("/api", api.Routes(deps)))

	handler := telemetry.RequestID(telemetry.AccessLog(logger)(mux))

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
	defer stopSweeper()
	go authSvc.RunSweeper(sweeperCtx, 0, logger)

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

	// Stop accepting, let in-flight requests finish, then let the deferred
	// closes run. Streaming responses get the same window, which is why the
	// timeout is generous rather than immediate.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown timed out", slog.String("error", err.Error()))
	}
	stopSweeper()
	logger.Info("stopped")
	return nil
}
