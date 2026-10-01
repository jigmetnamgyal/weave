package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestSecretLedgerErrorSafety tests private diagnostics and cancellation causes
// independently of the SQL driver; no private error chain is returned.
func TestSecretLedgerErrorSafety(t *testing.T) {
	poison := errors.New("synthetic private diagnostic")
	for _, tt := range []struct{ err, want error }{
		{nil, nil}, {poison, application.ErrSecretApprovalUnavailable},
		{pgx.ErrNoRows, application.ErrCredentialReferenceRejected},
		{&pgconn.PgError{Code: "23505", Message: "synthetic private uniqueness detail"}, application.ErrCredentialReferenceRejected},
		{&pgconn.PgError{Code: "23503"}, application.ErrCredentialReferenceRejected},
		{&pgconn.PgError{Code: "23514"}, application.ErrCredentialReferenceRejected},
		{&pgconn.PgError{Code: "42501"}, application.ErrSecretApprovalUnavailable},
		{domain.ErrInvalidSecretApproval, application.ErrCredentialReferenceRejected},
	} {
		got := secretLedgerError(context.Background(), tt.err)
		if got != tt.want || errors.Is(got, poison) {
			t.Fatal("unsafe diagnostic mapping")
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(poison)
	if got := secretLedgerError(ctx, poison); got != context.Canceled || errors.Is(got, poison) {
		t.Fatal("private cancellation cause exposed")
	}
}

// TestSecretLedgerConstructorsRejectMissingConfig ensures absence fails before I/O.
func TestSecretLedgerConstructorsRejectMissingConfig(t *testing.T) {
	if _, err := NewSecretApprovalStore(context.Background(), nil, "test", "918273645"); err != application.ErrCredentialConfiguration {
		t.Fatal(err)
	}
	if _, err := NewSecretApprovalWriter(context.Background(), nil, "test", "918273645", uuid.New()); err != application.ErrCredentialConfiguration {
		t.Fatal(err)
	}
	if err := ledgerScope(secretLedgerConfig{environment: "test", project: "918273645"}, domain.SecretApprovalScope{}); err != application.ErrCredentialReferenceRejected {
		t.Fatal(err)
	}
}

// TestSecretLedgerCancelledRollbackIntegration checks actual connection reuse
// after cancellation inside an owned transaction, without any customer writes.
func TestSecretLedgerCancelledRollbackIntegration(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("test DB config")
	}
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE weave_secret_approval_writer")
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	before, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pid := before.Conn().PgConn().PID()
	before.Release()
	ws := uuid.New()
	ctx, cancel := context.WithCancel(WithTenantWorkspace(context.Background(), ws))
	err = secretLedgerTx(ctx, pool, secretLedgerConfig{environment: "test", project: "918273645"}, ws, func(context.Context, *postgresdb.Queries) error { cancel(); return context.Canceled })
	if err != context.Canceled {
		t.Fatal("cancel response", err)
	}
	after, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer after.Release()
	if after.Conn().PgConn().PID() != pid {
		t.Fatal("canceled rollback discarded healthy connection")
	}
	if after.Conn().PgConn().TxStatus() != 'I' {
		t.Fatal("rollback left transaction open")
	}
}
