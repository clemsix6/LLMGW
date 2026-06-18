package postgres

import (
	"context"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// The methods below complete the domain.Store port but are implemented in later batches.
// They return errNotImplemented so the package compiles and the contract stays stable.

// LimitsFor is implemented in Batch 6.
func (s *Store) LimitsFor(ctx context.Context, projectID int64, tag string) ([]domain.BudgetLimit, error) {
	return nil, errNotImplemented
}

// PriceFor is implemented in Batch 5.
func (s *Store) PriceFor(ctx context.Context, model string) (in, out float64, ok bool, err error) {
	return 0, 0, false, errNotImplemented
}

// DefaultRoute is implemented in Batch 3.
func (s *Store) DefaultRoute(ctx context.Context) (domain.Provider, error) {
	return nil, errNotImplemented
}

// RecordUsage is implemented in Batch 3.
func (s *Store) RecordUsage(ctx context.Context, e domain.UsageEvent) error {
	return errNotImplemented
}

// WindowedTotals is implemented in Batch 3.
func (s *Store) WindowedTotals(ctx context.Context, projectID int64, tag string, since time.Time) (domain.Totals, error) {
	return domain.Totals{}, errNotImplemented
}

// Reserve is implemented in Batch 6.
func (s *Store) Reserve(ctx context.Context, projectID int64, tag string, ttl time.Duration) (reservationID int64, err error) {
	return 0, errNotImplemented
}

// ReleaseReservation is implemented in Batch 6.
func (s *Store) ReleaseReservation(ctx context.Context, reservationID int64) error {
	return errNotImplemented
}

// InflightTotals is implemented in Batch 6.
func (s *Store) InflightTotals(ctx context.Context, projectID int64, tag string) (domain.Totals, error) {
	return domain.Totals{}, errNotImplemented
}
