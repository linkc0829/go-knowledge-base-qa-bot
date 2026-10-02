package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/linkc0829/llm-platform/internal/auth"
	"github.com/linkc0829/llm-platform/internal/gateway"
	"github.com/linkc0829/llm-platform/internal/platform/config"
	"github.com/linkc0829/llm-platform/internal/platform/httpserver"
	"github.com/linkc0829/llm-platform/internal/platform/logger"
)

func main() {
	cfg, err := config.LoadGateway()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	logOutput := cfg.Gateway.LogOutput
	if logOutput == "" {
		logOutput = "stdout,log/gateway.log"
	}
	lg, err := logger.New(logger.Config{
		Level:    cfg.Logger.Level,
		Encoding: cfg.Logger.Encoding,
		Output:   logOutput,
	})
	if err != nil {
		log.Fatalf("logger: %v", err)
	}
	defer func() { _ = lg.Sync() }()
	zap.ReplaceGlobals(lg)

	authStore, err := auth.LoadFile(cfg.Auth.File, lg)
	if err != nil {
		lg.Sugar().Fatalf("auth: %v", err)
	}

	upstreamURL, err := url.Parse(cfg.Gateway.UpstreamBaseURL)
	if err != nil {
		lg.Sugar().Fatalf("upstream base url: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Dynamic auth reload: check mtime every 10 seconds.
	// ponytail: mtime poll, 10s revocation lag. Switch to an OIDC IdP when gateway goes multi-host.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reloaded, err := authStore.ReloadIfModified()
				if err != nil {
					lg.Warn("auth file reload check failed", zap.Error(err))
				} else if reloaded {
					lg.Info("auth file reloaded")
				}
			}
		}
	}()

	var embedURL *url.URL
	if cfg.Gateway.EmbedUpstreamBaseURL != "" {
		embedURL, err = url.Parse(cfg.Gateway.EmbedUpstreamBaseURL)
		if err != nil {
			lg.Sugar().Fatalf("embed upstream base url: %v", err)
		}
	}

	limiter := gateway.NewLimiter(cfg.Gateway.MaxInflightGlobal, cfg.Gateway.MaxInflightPerUser)
	h := gateway.NewHandler(
		upstreamURL, cfg.Gateway.UpstreamAPIKey,
		embedURL, cfg.Gateway.EmbedUpstreamAPIKey, cfg.Gateway.EmbedModel,
		limiter, authStore, cfg.Gateway.UpstreamHeaderTimeout, cfg.Gateway.MaxOutputTokens, lg,
	)

	engine := httpserver.New(lg)
	gateway.RegisterRoutes(engine.Group(""), h)

	// WriteTimeout: 0 disables the write deadline so long SSE streams are not truncated.
	zeroTimeout := time.Duration(0)
	srv := httpserver.Wrap(engine, httpserver.Config{
		Port:         cfg.Gateway.Port,
		BindAddress:  cfg.HTTP.BindAddress,
		WriteTimeout: &zeroTimeout,
	}, lg)

	errs := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	embedUpstreamStr := ""
	if embedURL != nil {
		embedUpstreamStr = embedURL.String()
	}
	lg.Info("gateway started",
		zap.Int("port", cfg.Gateway.Port),
		zap.String("upstream", upstreamURL.String()),
		zap.String("embed_upstream", embedUpstreamStr),
		zap.String("embed_model", cfg.Gateway.EmbedModel),
		zap.Duration("upstream_header_timeout", cfg.Gateway.UpstreamHeaderTimeout),
		zap.Int("max_inflight_global", cfg.Gateway.MaxInflightGlobal),
		zap.Int("max_inflight_per_user", cfg.Gateway.MaxInflightPerUser),
		zap.Int("max_output_tokens", cfg.Gateway.MaxOutputTokens),
	)

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil {
			lg.Sugar().Fatalf("server: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.App.ShutdownTimeout)
	defer cancel()

	_ = srv.Shutdown(shutdownCtx)
}
