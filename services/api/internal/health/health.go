// Package health implements the liveness and readiness probes for the API
// service.
//
// Liveness answers "is this process running?" and never touches a dependency.
// Readiness answers "can this process serve traffic?" and probes every backing
// dependency. Keeping them separate stops a transient dependency outage from
// causing an orchestrator to restart otherwise-healthy processes.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"sync"
)

// Status values reported for the probe as a whole and for each dependency.
const (
	StatusOK          = "ok"
	StatusUnavailable = "unavailable"
)

// ProbeFunc reports whether a single dependency is reachable. It must respect
// context cancellation so a slow dependency cannot stall the probe.
type ProbeFunc func(ctx context.Context) error

// Check pairs a dependency name with its probe.
type Check struct {
	Name  string
	Probe ProbeFunc
}

// dependencyStatus is the per-dependency section of a probe response.
//
// It deliberately carries no error detail: readiness output is reachable
// without authentication, and dependency errors routinely embed hostnames,
// ports and connection strings. The full error is logged instead.
type dependencyStatus struct {
	Status string `json:"status"`
}

// response is the JSON body returned by both probes.
type response struct {
	Status string                      `json:"status"`
	Checks map[string]dependencyStatus `json:"checks,omitempty"`
}

// Live returns a handler that reports process liveness. It performs no
// dependency I/O and answers as long as the process can serve HTTP.
func Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(r.Context(), w, http.StatusOK, response{Status: StatusOK})
	})
}

// Ready returns a handler that probes every check concurrently and reports
// the aggregate result. It responds 200 when all dependencies are reachable
// and 503 when any is not.
//
// Probes run in parallel because they are independent; the request context
// bounds the whole set, so total latency is that of the slowest probe rather
// than the sum.
func Ready(logger *slog.Logger, checks ...Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		results := make(map[string]dependencyStatus, len(checks))
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, check := range checks {
			wg.Add(1)
			go func(check Check) {
				defer wg.Done()

				status := StatusOK
				if err := check.Probe(ctx); err != nil {
					status = StatusUnavailable
					logger.ErrorContext(ctx, "readiness probe failed",
						slog.String("dependency", check.Name),
						slog.String("error", err.Error()),
					)
				}

				mu.Lock()
				defer mu.Unlock()
				results[check.Name] = dependencyStatus{Status: status}
			}(check)
		}

		wg.Wait()

		body := response{Status: StatusOK, Checks: results}
		code := http.StatusOK

		// Sorted iteration keeps the "first failing dependency" log line
		// deterministic across runs.
		names := make([]string, 0, len(results))
		for name := range results {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if results[name].Status != StatusOK {
				body.Status = StatusUnavailable
				code = http.StatusServiceUnavailable
				break
			}
		}

		writeJSON(ctx, w, code, body)
	})
}

// writeJSON serialises body with the given status code.
func writeJSON(ctx context.Context, w http.ResponseWriter, code int, body response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already written, so this can only be logged.
		slog.ErrorContext(ctx, "failed to encode health response", slog.String("error", err.Error()))
	}
}
