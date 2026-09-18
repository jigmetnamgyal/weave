package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	temporalclient "go.temporal.io/sdk/client"
)

// readinessTimeout bounds a probe. A dependency that has not answered in this
// long is not usable, and a probe that hangs is worse than one that fails.
const readinessTimeout = 3 * time.Second

// healthHandler reports whether the worker can do its job.
//
// The worker serves no product traffic, so this exists for the deployment
// system rather than for a user: without it a worker whose Temporal connection
// died later looks identical to one that is working, and nothing would replace
// it. Checking only at startup — which is what this did at first — makes the
// process healthy forever from the moment it starts.
//
// Both dependencies are hard here, unlike in the API. The worker cannot drain
// the outbox without PostgreSQL and cannot start a workflow without Temporal,
// so either being unreachable means it is running and useless.
func healthHandler(pool *pgxpool.Pool, client temporalclient.Client, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Liveness answers "is this process running", with no dependency I/O — a
	// dependency outage must not get the worker killed and restarted into the
	// same outage.
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		status := map[string]string{"postgres": "ok", "temporal": "ok"}
		ready := true

		if err := pool.Ping(ctx); err != nil {
			status["postgres"] = "unavailable"
			ready = false
			// The error is logged and not returned: probe output is
			// unauthenticated, and a connection error carries hostnames.
			logger.WarnContext(ctx, "readiness: postgres is unreachable",
				slog.String("error", err.Error()))
		}

		if _, err := client.CheckHealth(ctx, &temporalclient.CheckHealthRequest{}); err != nil {
			status["temporal"] = "unavailable"
			ready = false
			logger.WarnContext(ctx, "readiness: temporal is unreachable",
				slog.String("error", err.Error()))
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
