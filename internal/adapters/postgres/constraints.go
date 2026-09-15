package postgres

import (
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// constraintViolated reports whether err is PostgreSQL rejecting a named
// constraint.
//
// Matching on the constraint name rather than the message, because the message
// is prose PostgreSQL is free to reword between versions while the name is
// something this repository chose and controls.
func constraintViolated(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == name
}

// nullableUUID renders an optional identifier for a nullable column.
func nullableUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
