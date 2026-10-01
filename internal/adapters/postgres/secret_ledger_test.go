package postgres

import (
	"context"
	"errors"
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
