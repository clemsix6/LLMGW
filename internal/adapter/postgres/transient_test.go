package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsTransientUsageError(t *testing.T) {
	for _, code := range []string{
		"08000", "08001", "08003", "08006", "08007",
		"40001", "40P01", "55P03", "53300",
		"57P01", "57P02", "57P03",
	} {
		t.Run("SQLSTATE "+code, func(t *testing.T) {
			err := fmt.Errorf("repository wrapper: %w", &pgconn.PgError{Code: code})
			if !IsTransientUsageError(err) {
				t.Fatalf("IsTransientUsageError(SQLSTATE %s) = false, want true", code)
			}
		})
	}

	for _, test := range []struct {
		name string
		err  error
	}{
		{"context canceled", fmt.Errorf("wrapped: %w", context.Canceled)},
		{"context deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded)},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}},
		{"unique violation", &pgconn.PgError{Code: "23505"}},
		{"protocol violation", &pgconn.PgError{Code: "08P01"}},
		{"server rejected connection", &pgconn.PgError{Code: "08004"}},
		{"configuration limit", &pgconn.PgError{Code: "53400"}},
		{"no rows", pgx.ErrNoRows},
		{"opaque", errors.New("transient-looking words are not types")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if IsTransientUsageError(test.err) {
				t.Fatalf("IsTransientUsageError(%v) = true, want false", test.err)
			}
		})
	}

	if !IsTransientUsageError(fmt.Errorf("wrapped network: %w", timeoutNetworkError{})) {
		t.Fatal("wrapped network timeout is not transient")
	}
}

type timeoutNetworkError struct{}

func (timeoutNetworkError) Error() string   { return "network timeout" }
func (timeoutNetworkError) Timeout() bool   { return true }
func (timeoutNetworkError) Temporary() bool { return true }
