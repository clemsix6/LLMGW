package integration

import (
	"context"
	"sync"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// gatedUsageRepository delegates to PostgreSQL and can pause RecordAttempt
// without blocking the client-facing SDK handler.
type gatedUsageRepository struct {
	store *postgres.Store

	mu   sync.Mutex
	gate *usagePersistenceGate
}

type usagePersistenceGate struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newGatedUsageRepository(store *postgres.Store) *gatedUsageRepository {
	return &gatedUsageRepository{store: store}
}

func (r *gatedUsageRepository) block() (<-chan struct{}, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gate := &usagePersistenceGate{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	r.gate = gate
	return gate.entered, func() {
		gate.releaseOnce.Do(func() {
			close(gate.release)
			r.mu.Lock()
			if r.gate == gate {
				r.gate = nil
			}
			r.mu.Unlock()
		})
	}
}

func (r *gatedUsageRepository) PriceRuleFor(
	ctx context.Context,
	provider string,
	model string,
	tier string,
	at time.Time,
) (governance.PriceRule, bool, error) {
	return r.store.PriceRuleFor(ctx, provider, model, tier, at)
}

func (r *gatedUsageRepository) RecordAttempt(
	ctx context.Context,
	attempt governance.UsageAttempt,
) error {
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		gate.enteredOnce.Do(func() { close(gate.entered) })
		select {
		case <-gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.store.RecordAttempt(ctx, attempt)
}
