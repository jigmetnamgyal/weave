module github.com/jigmetnamgyal/weave

go 1.25.0

// The `go` directive above is the minimum language version our dependencies
// require. This pins the toolchain that actually builds and tests the module:
// go1.25.0 ships with known standard-library vulnerabilities (crypto/x509,
// crypto/tls and others) that govulncheck flags, and 1.25.14 is the current
// patched release of that line. Keep in sync with versions.env.
toolchain go1.25.14

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/nats-io/nats.go v1.53.1
	github.com/redis/go-redis/v9 v9.22.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)
