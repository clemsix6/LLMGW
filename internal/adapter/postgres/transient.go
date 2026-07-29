package postgres

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsTransientUsageError classifies only typed transport and explicitly
// retryable PostgreSQL failures. Context and data errors fail permanently.
func IsTransientUsageError(err error) bool {
	if err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return transientSQLState(postgresError.Code)
	}
	if pgconn.SafeToRetry(err) ||
		pgconn.Timeout(err) ||
		errors.Is(err, pgconn.ErrConnClosed) {
		return true
	}

	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

func transientSQLState(code string) bool {
	switch code {
	case
		"08000", // connection_exception
		"08001", // sqlclient_unable_to_establish_sqlconnection
		"08003", // connection_does_not_exist
		"08006", // connection_failure
		"08007", // transaction_resolution_unknown
		"40001", // serialization_failure
		"40P01", // deadlock_detected
		"55P03", // lock_not_available
		"53300", // too_many_connections
		"57P01", // admin_shutdown
		"57P02", // crash_shutdown
		"57P03": // cannot_connect_now
		return true
	default:
		return false
	}
}
