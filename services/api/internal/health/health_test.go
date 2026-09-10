package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// discardLogger keeps probe-failure logging out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func okProbe(context.Context) error { return nil }

func failProbe(context.Context) error {
	return errors.New("dial tcp 10.1.2.3:5432: connection refused")
}

func TestLive(t *testing.T) {
	rec := httptest.NewRecorder()
	Live().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != StatusOK {
		t.Errorf("status = %q, want %q", body.Status, StatusOK)
	}
	if len(body.Checks) != 0 {
		t.Errorf("liveness returned dependency checks %v, want none", body.Checks)
	}
}

func TestReady(t *testing.T) {
	tests := []struct {
		name       string
		checks     []Check
		wantCode   int
		wantStatus string
		wantChecks map[string]string
	}{
		{
			name: "all dependencies reachable",
			checks: []Check{
				{Name: "postgres", Probe: okProbe},
				{Name: "redis", Probe: okProbe},
				{Name: "nats", Probe: okProbe},
			},
			wantCode:   http.StatusOK,
			wantStatus: StatusOK,
			wantChecks: map[string]string{
				"postgres": StatusOK,
				"redis":    StatusOK,
				"nats":     StatusOK,
			},
		},
		{
			name: "one unreachable dependency fails the probe",
			checks: []Check{
				{Name: "postgres", Probe: okProbe},
				{Name: "redis", Probe: failProbe},
				{Name: "nats", Probe: okProbe},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: StatusUnavailable,
			wantChecks: map[string]string{
				"postgres": StatusOK,
				"redis":    StatusUnavailable,
				"nats":     StatusOK,
			},
		},
		{
			name: "every dependency unreachable",
			checks: []Check{
				{Name: "postgres", Probe: failProbe},
				{Name: "redis", Probe: failProbe},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: StatusUnavailable,
			wantChecks: map[string]string{
				"postgres": StatusUnavailable,
				"redis":    StatusUnavailable,
			},
		},
		{
			name:       "no checks registered is ready",
			checks:     nil,
			wantCode:   http.StatusOK,
			wantStatus: StatusOK,
			wantChecks: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler := Ready(discardLogger(), tt.checks...)
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}

			var body response
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", body.Status, tt.wantStatus)
			}
			if len(body.Checks) != len(tt.wantChecks) {
				t.Fatalf("checks = %v, want %d entries", body.Checks, len(tt.wantChecks))
			}
			for name, want := range tt.wantChecks {
				got, ok := body.Checks[name]
				if !ok {
					t.Errorf("missing check %q in %v", name, body.Checks)
					continue
				}
				if got.Status != want {
					t.Errorf("check %q = %q, want %q", name, got.Status, want)
				}
			}
		})
	}
}

// TestReadyDoesNotLeakDependencyDetail guards the rule that connection detail
// stays in logs and out of the unauthenticated probe body.
func TestReadyDoesNotLeakDependencyDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	handler := Ready(discardLogger(), Check{Name: "postgres", Probe: failProbe})
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

	for _, leaked := range []string{"10.1.2.3", "5432", "connection refused"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("response body %q leaked dependency detail %q", rec.Body.String(), leaked)
		}
	}
}

// TestReadyRunsProbesConcurrently proves probes are not serialised: three
// probes that each sleep would exceed the deadline if run one after another.
func TestReadyRunsProbesConcurrently(t *testing.T) {
	const probeDelay = 100 * time.Millisecond

	var running, maxConcurrent atomic.Int64
	slowProbe := func(ctx context.Context) error {
		current := running.Add(1)
		for {
			observed := maxConcurrent.Load()
			if current <= observed || maxConcurrent.CompareAndSwap(observed, current) {
				break
			}
		}
		defer running.Add(-1)

		select {
		case <-time.After(probeDelay):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	checks := []Check{
		{Name: "postgres", Probe: slowProbe},
		{Name: "redis", Probe: slowProbe},
		{Name: "nats", Probe: slowProbe},
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	Ready(discardLogger(), checks...).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := maxConcurrent.Load(); got != int64(len(checks)) {
		t.Errorf("max concurrent probes = %d, want %d", got, len(checks))
	}
	if elapsed >= probeDelay*time.Duration(len(checks)) {
		t.Errorf("probes took %v, which indicates serial execution", elapsed)
	}
}

// TestReadyHonoursContextCancellation proves a probe that respects its context
// surfaces as unavailable rather than hanging the request.
func TestReadyHonoursContextCancellation(t *testing.T) {
	blockingProbe := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil).WithContext(ctx)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		Ready(discardLogger(), Check{Name: "postgres", Probe: blockingProbe}).ServeHTTP(rec, req)
		done <- rec.Code
	}()

	cancel()

	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", code, http.StatusServiceUnavailable)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readiness handler did not return after context cancellation")
	}
}
