// Command egress-proxy forwards runner sandboxes' requests to workspace-added
// hosts (Unit M5.4d.3a, ADR-017).
//
// Vercel's firewall sends each added host's traffic here through its forwardURL
// rule. Each request is authenticated by the OIDC token Vercel signs for the
// sandbox, authorized against its runner's own immutable snapshot, and fetched
// through the guarded transport, which connects only to validated public
// addresses — refused rather than forwarded if any of that fails. The third
// public edge, after the event ingress (ADR-015) and registry proxy (ADR-016).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/services/egress-proxy/internal/config"
	"github.com/jigmetnamgyal/weave/services/egress-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "weave-egress-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.AppDatabaseURL)
	if err != nil {
		return fmt.Errorf("configure application postgres pool: %w", err)
	}
	defer pool.Close()

	verifier, err := vercelsandbox.NewOIDCVerifier(ctx, vercelsandbox.OIDCConfig{
		TeamID: cfg.VercelTeamID, ProjectID: cfg.VercelProjectID,
	})
	if err != nil {
		return err
	}
	authorizer := postgres.NewEgressAuthorizer(pool, vercelsandbox.BackendName+"-")
	handler := proxy.New(proxy.Config{PublicBase: cfg.PublicURL, Verifier: verifier, Authorizer: authorizer, Logger: logger})

	// No write timeout: a large download streams for as long as it streams. The
	// header and read timeouts bound a client that never finishes its request,
	// and the idle timeout a connection left open.
	// ReadTimeout backstops the handler's own body deadline: reading a whole
	// request — headers and body — may take no longer. It bounds reads only,
	// never the streamed response.
	public := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: proxy.DefaultBodyReadTimeout + 10*time.Second, IdleTimeout: 2 * time.Minute}
	health := &http.Server{Handler: healthHandler(pool, verifier, logger), ReadHeaderTimeout: 5 * time.Second}

	failed := make(chan error, 2)
	for _, s := range []struct {
		server *http.Server
		addr   string
		name   string
	}{{public, cfg.Addr, "public"}, {health, cfg.HealthAddr, "health"}} {
		listener, err := net.Listen("tcp", s.addr)
		if err != nil {
			return fmt.Errorf("bind egress-proxy %s listener on %s: %w", s.name, s.addr, err)
		}
		go func(server *http.Server, name string) {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("%s listener: %w", name, err)
			}
		}(s.server, s.name)
	}
	logger.InfoContext(ctx, "egress proxy started",
		slog.String("addr", cfg.Addr), slog.String("health", cfg.HealthAddr), slog.String("public_url", cfg.PublicURL))

	select {
	case <-ctx.Done():
	case err := <-failed:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdown)
	_ = health.Shutdown(shutdown)
	return nil
}

// healthHandler: live always; ready when PostgreSQL answers and Vercel's keys
// have been fetched — without keys every request would be refused.
func healthHandler(pool *pgxpool.Pool, verifier *vercelsandbox.OIDCVerifier, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		status, ready := map[string]string{"postgres": "ok", "vercel_keys": "ok"}, true
		if err := pool.Ping(ctx); err != nil {
			status["postgres"], ready = "unavailable", false
			logger.WarnContext(ctx, "readiness: postgres is unreachable", slog.String("error", err.Error()))
		}
		if !verifier.Ready(ctx) {
			status["vercel_keys"], ready = "unavailable", false
			logger.WarnContext(ctx, "readiness: Vercel's signing keys are not fetched")
		}
		code := http.StatusOK
		if !ready {
			code = http.StatusServiceUnavailable
		}
		writeHealth(w, code, status)
	})
	return mux
}

func writeHealth(w http.ResponseWriter, code int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
