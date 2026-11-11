package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	codeUniqueViolation           = "23505"
	codeForeignKeyViolation       = "23503"
	codeNotNullViolation          = "23502"
	codeCheckViolation            = "23514"
	codeInvalidTextRepresentation = "22P02"
	codeStringDataRightTruncation = "22001"
	codeSerializationFailure      = "40001"
	codeDeadlockDetected          = "40P01"
)

func notFound(resource, id string) error {
	return &domain.NotFoundError{Resource: resource, ID: id}
}

// mapError converts PostgreSQL constraint and concurrency errors into the
// domain error types expected by callers.
func mapError(err error, resource, id string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(resource, id)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case codeUniqueViolation:
		return &domain.ConflictError{Resource: resource, ID: id, Reason: "duplicate key"}
	case codeForeignKeyViolation:
		return &domain.ValidationError{Field: "reference", Reason: "referenced entity does not exist"}
	case codeNotNullViolation:
		return &domain.ValidationError{Field: pgErr.ColumnName, Reason: "must not be null"}
	case codeCheckViolation:
		return &domain.ValidationError{Field: pgErr.ConstraintName, Reason: "constraint violated"}
	case codeInvalidTextRepresentation, codeStringDataRightTruncation:
		return &domain.ValidationError{Field: pgErr.ColumnName, Reason: "invalid value"}
	case codeSerializationFailure, codeDeadlockDetected:
		return fmt.Errorf("%w: %s", domain.ErrConflict, pgErr.Message)
	}
	return err
}
