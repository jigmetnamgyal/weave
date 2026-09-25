package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

// readinessTimeout bounds a probe; a dependency that has not answered in this
// long is not usable.
const readinessTimeout = 3 * time.Second

// healthHandler reports whether the ingestor can do its job.
//
// Both dependencies are hard: without NATS there is nothing to ingest, and
// without PostgreSQL nothing can be stored or quarantined. Temporal is not
// here at all — a Temporal outage says nothing about whether events can be
// ingested, which is one reason this is its own process.
func healthHandler(pool *pgxpool.Pool, conn *nats.Conn, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Liveness does no dependency I/O, so an outage does not get the process
	// restarted into the same outage.
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		status := map[string]string{"postgres": "ok", "nats": "ok"}
		ready := true

		if err := pool.Ping(ctx); err != nil {
			status["postgres"] = "unavailable"
			ready = false
			// Logged, not returned: probe output is unauthenticated and a
			// connection error carries hostnames.
			logger.WarnContext(ctx, "readiness: postgres is unreachable", slog.String("error", err.Error()))
		}
		if !conn.IsConnected() {
			status["nats"] = "unavailable"
			ready = false
			logger.WarnContext(ctx, "readiness: nats is not connected", slog.String("status", conn.Status().String()))
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
