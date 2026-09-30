package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.uber.org/zap"

	"github.com/linkc0829/llm-platform/internal/auth"
	"github.com/linkc0829/llm-platform/internal/bootstrap"
	"github.com/linkc0829/llm-platform/internal/kb"
	"github.com/linkc0829/llm-platform/internal/mcpserver"
	"github.com/linkc0829/llm-platform/internal/platform/config"
	"github.com/linkc0829/llm-platform/internal/platform/httpserver"
	"github.com/linkc0829/llm-platform/internal/platform/logger"
)

func main() {
	// Four eval rounds were measured against a service still running an older
	// grounding prompt, because a rebuilt binary sitting in bin/ looks exactly
	// like a restarted one. Printing the fingerprint the binary carries makes
	// the comparison against a live /health a single command.
	fingerprint := flag.Bool("fingerprint", false, "print the grounding prompt fingerprint and exit")
	flag.Parse()
	if *fingerprint {
		fmt.Fprintln(os.Stdout, kb.GroundingFingerprint())
		return
	}

	cfg, err := config.LoadKB()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	lg, err := logger.New(logger.Config{Level: cfg.Logger.Level, Encoding: cfg.Logger.Encoding, Output: cfg.Logger.Output})
	if err != nil {
		log.Fatalf("logger: %v", err)
	}

	// Validate the auth snapshot before constructing the service or listener.
	// The service logs each answered query through zap's global after validation.
	var authStore *auth.Store
	if !cfg.Auth.Disabled {
		authStore, err = auth.LoadFile(cfg.Auth.File, lg)
		if err != nil {
			log.Fatalf("auth: %v", err)
		}
	} else {
		lg.Warn("authentication disabled; forcing localhost bind")
	}
	zap.ReplaceGlobals(lg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if strings.EqualFold(cfg.OpenAI.LLMMode, "fake") {
		lg.Warn("using fake LLM mode; responses are deterministic and do not call OpenAI")
	}
	services := bootstrap.NewServices(cfg, authStore)
	svc := services.KB
	if err := svc.LoadOnStartup(ctx); err != nil {
		switch {
		case errors.Is(err, kb.ErrNotIndexed):
			lg.Warn("knowledge base not indexed yet; POST /index to build it")
		case errors.Is(err, kb.ErrIndexStale):
			lg.Warn("index is stale; POST /index to rebuild it")
		case errors.Is(err, kb.ErrIndexAccessAuditFailed):
			lg.Warn("persisted index failed access audit; POST /index to rebuild it")
		case errors.Is(err, kb.ErrVectorsIgnored):
			lg.Warn("vector index is stale; POST /index to rebuild it")
		default:
			lg.Sugar().Fatalf("load index: %v", err)
		}
	}
	h := kb.NewHandler(svc, lg, httpPrincipalProvider(cfg), bootstrap.ChatRuntime(cfg), cfg.KB.IndexTimeout, cfg.KB.ChatTimeout)

	routeGuards := kb.RouteGuards{AllowUnauthenticated: cfg.Auth.Disabled}
	if !cfg.Auth.Disabled {
		routeGuards = kb.RouteGuards{
			Authenticate:   auth.RequirePrincipal(services.Auth),
			RequireIndexer: auth.RequireIndexer(),
		}
	}

	engine := httpserver.New(lg)
	kb.RegisterRoutes(engine.Group(""), h, routeGuards)
	if !cfg.Auth.Disabled {
		auth.RegisterTokenRoutes(engine.Group(""), auth.NewHandler(services.Auth, auth.PrincipalIDFromContext), auth.TokenRouteGuards{
			Authenticate: auth.RequirePrincipal(services.Auth),
			RequireAdmin: auth.RequireAdmin(),
		})
	}
	mcpserver.RegisterStreamableHTTPRoutes(engine, svc, lg, cfg.Auth.Disabled, services.Auth)

	srv := httpserver.Wrap(engine, bootstrap.KBServerConfig(cfg, httpBindAddress(cfg)), lg)

	errs := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

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
	_ = lg.Sync()
}
