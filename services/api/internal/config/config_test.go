package config

import (
	"errors"
	"strings"
	"testing"
)

// validEnv is the smallest environment that Load accepts.
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":       "postgres://weave:weave@localhost:5432/weave?sslmode=disable",
		"REDIS_URL":          "redis://localhost:6379/0",
		"NATS_URL":           "nats://localhost:4222",
		"TEMPORAL_HOST_PORT": "localhost:7233",
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		unset     []string
		wantErr   error
		errSubstr []string
		assert    func(t *testing.T, cfg Config)
	}{
		{
			name: "required variables only, defaults applied",
			env:  validEnv(),
			assert: func(t *testing.T, cfg Config) {
				if cfg.AppEnv != "development" {
					t.Errorf("AppEnv = %q, want development", cfg.AppEnv)
				}
				if cfg.HTTPAddr != ":8080" {
					t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
				}
				if cfg.OTelExporter != "none" {
					t.Errorf("OTelExporter = %q, want none", cfg.OTelExporter)
				}
				if cfg.OTelServiceName != "weave-api" {
					t.Errorf("OTelServiceName = %q, want weave-api", cfg.OTelServiceName)
				}
			},
		},
		{
			name:      "single missing required variable is named",
			env:       validEnv(),
			unset:     []string{"REDIS_URL"},
			wantErr:   ErrMissingConfig,
			errSubstr: []string{"REDIS_URL", ".env.example"},
		},
		{
			name:      "every missing variable is reported at once",
			env:       map[string]string{},
			unset:     []string{"DATABASE_URL", "REDIS_URL", "NATS_URL", "TEMPORAL_HOST_PORT"},
			wantErr:   ErrMissingConfig,
			errSubstr: []string{"DATABASE_URL", "NATS_URL", "REDIS_URL", "TEMPORAL_HOST_PORT"},
		},
		{
			name: "blank value counts as missing",
			env: func() map[string]string {
				e := validEnv()
				e["NATS_URL"] = "   "
				return e
			}(),
			wantErr:   ErrMissingConfig,
			errSubstr: []string{"NATS_URL"},
		},
		{
			name: "overrides are honoured and trimmed",
			env: func() map[string]string {
				e := validEnv()
				e["APP_ENV"] = "test"
				e["API_HTTP_ADDR"] = "  :9999  "
				e["OTEL_EXPORTER"] = "stdout"
				e["OTEL_SERVICE_NAME"] = "weave-api-test"
				return e
			}(),
			assert: func(t *testing.T, cfg Config) {
				if cfg.AppEnv != "test" {
					t.Errorf("AppEnv = %q, want test", cfg.AppEnv)
				}
				if cfg.HTTPAddr != ":9999" {
					t.Errorf("HTTPAddr = %q, want :9999", cfg.HTTPAddr)
				}
				if cfg.OTelExporter != "stdout" {
					t.Errorf("OTelExporter = %q, want stdout", cfg.OTelExporter)
				}
			},
		},
		{
			name: "unknown exporter is rejected",
			env: func() map[string]string {
				e := validEnv()
				e["OTEL_EXPORTER"] = "jaeger"
				return e
			}(),
			errSubstr: []string{"OTEL_EXPORTER", "jaeger"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv restores the previous value when the subtest ends.
			for _, key := range append(requiredKeys, "APP_ENV", "API_HTTP_ADDR", "OTEL_EXPORTER", "OTEL_SERVICE_NAME") {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			for _, key := range tt.unset {
				t.Setenv(key, "")
			}

			cfg, err := Load()

			if tt.wantErr == nil && len(tt.errSubstr) == 0 {
				if err != nil {
					t.Fatalf("Load() returned unexpected error: %v", err)
				}
				if tt.assert != nil {
					tt.assert(t, cfg)
				}
				return
			}

			if err == nil {
				t.Fatal("Load() returned nil error, want failure")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Load() error = %v, want errors.Is(%v)", err, tt.wantErr)
			}
			for _, substr := range tt.errSubstr {
				if !strings.Contains(err.Error(), substr) {
					t.Errorf("Load() error %q does not mention %q", err.Error(), substr)
				}
			}
		})
	}
}
