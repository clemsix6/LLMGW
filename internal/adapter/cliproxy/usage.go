package cliproxy

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/cost"
	"github.com/google/uuid"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	// usageTimeout bounds one detached plugin callback.
	usageTimeout = 5 * time.Second
	// usageAttempts bounds repository attempts for one SDK record.
	usageAttempts = 3
)

// UsagePlugin normalizes and durably persists SDK usage callbacks.
type UsagePlugin struct {
	repo      usageAttemptRepository // repo resolves pricing and persists attempts.
	bridge    *UsageBridge           // bridge verifies immutable SDK principals.
	retryable func(error) bool       // retryable classifies repository errors.
}

// usageAttemptRepository is the Task 7 subset of the future complete usage port.
type usageAttemptRepository interface {
	// PriceRuleFor resolves pricing effective for one SDK attempt.
	PriceRuleFor(context.Context, string, string, string, time.Time) (governance.PriceRule, bool, error)
	// RecordAttempt durably persists one normalized SDK attempt.
	RecordAttempt(context.Context, governance.UsageAttempt) error
}

// NewUsagePlugin constructs the durable LLMGW SDK usage plugin.
func NewUsagePlugin(
	repo usageAttemptRepository,
	bridge *UsageBridge,
	retryable func(error) bool,
) *UsagePlugin {
	return &UsagePlugin{
		repo:      repo,
		bridge:    bridge,
		retryable: retryable,
	}
}

// HandleUsage persists one attempt without propagating callback panics.
func (p *UsagePlugin) HandleUsage(_ context.Context, record sdkusage.Record) {
	requestID := ""
	defer func() {
		if recover() == nil {
			return
		}
		if requestID == "" {
			p.bridge.poison()
		} else {
			p.bridge.fail(requestID)
		}
		log.Print("llmgw: usage plugin panic recovered")
	}()

	correlation, ok := p.bridge.correlation(record.APIKey)
	if !ok {
		if canceledRequestID, isCancel := p.bridge.cancelRequestID(record.APIKey); isCancel {
			requestID = canceledRequestID
			p.bridge.markCanceled(canceledRequestID)
			return
		}
		if barrierRequestID, canceled, isBarrier := p.bridge.barrierState(record.APIKey); isBarrier {
			requestID = barrierRequestID
			p.bridge.completeBarrier(barrierRequestID, canceled)
			return
		}
		p.bridge.poison()
		log.Print("llmgw: reject usage callback: invalid authenticated purpose")
		return
	}
	requestID = correlation.requestID
	if !p.bridge.acceptRecord(requestID) {
		log.Print("llmgw: reject usage callback: released authenticated group")
		return
	}
	if record.RequestedAt.IsZero() {
		p.bridge.fail(requestID)
		log.Printf("llmgw: reject usage callback (request=%s): requested time is required", requestID)
		return
	}
	attempt := mapUsageRecord(record, correlation, uuid.NewString())

	persistCtx, cancel := context.WithTimeout(context.Background(), usageTimeout)
	defer cancel()

	if p.persist(persistCtx, record, attempt) {
		p.bridge.persisted(correlation.requestID, record.Failed)
		return
	}
	p.bridge.fail(correlation.requestID)
	log.Printf("llmgw: persist usage attempt (request=%s): unavailable", correlation.requestID)
}

// persist resolves price and writes one stable attempt with bounded retries.
func (p *UsagePlugin) persist(
	ctx context.Context,
	record sdkusage.Record,
	attempt governance.UsageAttempt,
) bool {
	for retry := 0; retry < usageAttempts; retry++ {
		priced, err := p.price(ctx, record, attempt)
		if err == nil {
			err = p.repo.RecordAttempt(ctx, priced)
		}
		if err == nil {
			return true
		}
		if p.retryable == nil || !p.retryable(err) ||
			retry+1 == usageAttempts || !waitUsageRetry(ctx, retry) {
			return false
		}
	}
	return false
}

// price resolves effective notional pricing without rejecting unknown prices.
func (p *UsagePlugin) price(
	ctx context.Context,
	record sdkusage.Record,
	attempt governance.UsageAttempt,
) (governance.UsageAttempt, error) {
	rule, found, err := p.repo.PriceRuleFor(
		ctx,
		attempt.Provider,
		attempt.ResolvedModel,
		pricingTier(attempt),
		attempt.CreatedAt,
	)
	if err != nil {
		return attempt, err
	}

	detail := sdkusage.EnsureTokenBreakdownForProvider(
		record.Detail,
		record.Provider,
		record.ExecutorType,
	)
	value, known := cost.Calculate(attempt.Tokens, rule)
	if found && detail.TokenBreakdown.Quality == sdkusage.TokenAccountingQualityComplete && known {
		attempt.CostUSD = &value
		attempt.PricingState = governance.PricingPriced
		return attempt, nil
	}
	attempt.CostUSD = nil
	attempt.PricingState = governance.PricingUnknown
	return attempt, nil
}

// pricingTier prefers an observed upstream tier over request semantics.
func pricingTier(attempt governance.UsageAttempt) string {
	if tier := strings.TrimSpace(attempt.ResponseServiceTier); tier != "" {
		return tier
	}
	return attempt.ServiceTier
}

// waitUsageRetry waits for the next 50ms or 100ms retry.
func waitUsageRetry(ctx context.Context, retry int) bool {
	delay := 50 * time.Millisecond * time.Duration(retry+1)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
