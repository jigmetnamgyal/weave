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
		"APP_DATABASE_URL":   "postgres://weave_app:weave_app@localhost:5432/weave?sslmode=disable",
		"REDIS_URL":          "redis://localhost:6379/0",
		"NATS_URL":           "nats://localhost:4222",
		"TEMPORAL_HOST_PORT": "localhost:7233",
		"CLERK_ISSUER":       "https://test.clerk.accounts.dev",

		"GITHUB_APP_ID":               "4906626",
		"GITHUB_APP_SLUG":             "weave-test",
		"GITHUB_APP_PRIVATE_KEY_PATH": "./secrets/github-app.pem",
		"GITHUB_APP_WEBHOOK_SECRET":   "0123456789abcdef",
	}
}

// TestLoad verifies required values, defaults, overrides, and invalid configuration.
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
			name:    "every missing variable is reported at once",
			env:     map[string]string{},
			unset:   requiredKeys,
			wantErr: ErrMissingConfig,
			errSubstr: []string{
				"APP_DATABASE_URL", "CLERK_ISSUER", "DATABASE_URL", "NATS_URL", "REDIS_URL", "TEMPORAL_HOST_PORT",
				"GITHUB_APP_ID", "GITHUB_APP_SLUG", "GITHUB_APP_PRIVATE_KEY_PATH", "GITHUB_APP_WEBHOOK_SECRET",
			},
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
			name: "required values are trimmed before they are stored",
			env: func() map[string]string {
				e := validEnv()
				for key, value := range e {
					e[key] = "  " + value + "\t\n"
				}
				return e
			}(),
			assert: func(t *testing.T, cfg Config) {
				want := validEnv()
				got := map[string]string{
					"DATABASE_URL":       cfg.DatabaseURL,
					"REDIS_URL":          cfg.RedisURL,
					"NATS_URL":           cfg.NATSURL,
					"TEMPORAL_HOST_PORT": cfg.TemporalHostPort,
				}
				for key, value := range got {
					if value != want[key] {
						t.Errorf("%s = %q, want %q", key, value, want[key])
					}
				}
			},
		},
		{
			// .env.example must carry non-empty values so the required-key
			// check passes in CI. Accepting them would mean a copied template
			// starts an API whose webhook secret is published in this
			// repository, and anyone could forge a signed delivery.
			name: "a placeholder webhook secret is rejected",
			env: func() map[string]string {
				e := validEnv()
				e["GITHUB_APP_WEBHOOK_SECRET"] = "replace-me"
				return e
			}(),
			errSubstr: []string{"GITHUB_APP_WEBHOOK_SECRET", "placeholder"},
		},
		{
			name: "a placeholder is rejected whatever its case",
			env: func() map[string]string {
				e := validEnv()
				e["GITHUB_APP_SLUG"] = "Replace-Me"
				return e
			}(),
			errSubstr: []string{"GITHUB_APP_SLUG"},
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
			optional := []string{"APP_ENV", "API_HTTP_ADDR", "OTEL_EXPORTER", "OTEL_SERVICE_NAME", "CLERK_JWT_AUDIENCE", "GITHUB_WEBHOOK_PROXY_URL"}
			for _, key := range append(append([]string{}, requiredKeys...), optional...) {
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
