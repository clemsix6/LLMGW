package codex

import (
	"context"
	"log"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// defaultCooldown is applied on a 429 with no reset hint and on transient upstream failures
	// (5xx / auth). Deliberately short so a transient limit clears quickly and the account
	// returns to the pool.
	defaultCooldown = 60 * time.Second

	// deadTokenCooldown benches an account whose credential can't be refreshed (a dead OAuth
	// token needing an operator re-seed); cooling it longer avoids hammering the OAuth endpoint
	// each Send.
	deadTokenCooldown = 15 * time.Minute
)

// accountStore is the persistence the multi-account pool needs: per-account token access (for
// the token manager) plus the account roster and cooldown state that drives round-robin selection.
type accountStore interface {
	tokenStore

	// LoadAccounts returns every account for the named provider with its cooldown state, ordered
	// by label.
	LoadAccounts(ctx context.Context, providerName string) ([]domain.Account, error)

	// SetCooldown records that an account is rate-limited until the given time under the named
	// provider.
	SetCooldown(ctx context.Context, providerName, account string, until time.Time) error
}

// selectOrder returns the labels of the non-cooling accounts in round-robin order: it advances the
// cursor once per call and starts the scan there, so consecutive Sends prefer different accounts.
// An empty result means every account is cooling (or the pool is empty).
func (p *Provider) selectOrder(accounts []domain.Account, now time.Time) []string {
	n := len(accounts)
	if n == 0 {
		return nil
	}

	start := int((p.next.Add(1) - 1) % uint64(n))

	var order []string
	for i := 0; i < n; i++ {
		account := accounts[(start+i)%n]
		if !cooling(account, now) {
			order = append(order, account.Label)
		}
	}
	return order
}

// cooling reports whether an account is still on cooldown at now.
func cooling(account domain.Account, now time.Time) bool {
	return !account.CooldownUntil.IsZero() && account.CooldownUntil.After(now)
}

// cool puts an account on cooldown until the given time and persists it so the next Send's
// selectOrder skips the account. The caller (via cooldownFor) picks the duration per failure type.
// A persistence failure is logged but not fatal — the next Send simply re-reads the (still
// un-cooled) account and tries it again.
func (p *Provider) cool(ctx context.Context, account string, until time.Time) {
	if err := p.store.SetCooldown(ctx, p.providerName, account, until); err != nil {
		log.Printf("llmgw: codex set cooldown for %q: %v", account, err)
	}
}

// allCooling builds the AllCoolingError returned when no account could serve the request: it
// re-reads the pool and sets RetryAfter to the delay until the soonest cooldown clears, falling
// back to the default when the pool can't be read or carries no cooldown.
func (p *Provider) allCooling(ctx context.Context, now time.Time) error {
	accounts, err := p.store.LoadAccounts(ctx, p.providerName)
	if err != nil {
		return &AllCoolingError{After: defaultCooldown}
	}

	return &AllCoolingError{After: retryAfterUntil(soonestCooldown(accounts), now)}
}

// soonestCooldown returns the earliest non-zero cooldown_until across the accounts, or the zero
// time when none is set.
func soonestCooldown(accounts []domain.Account) time.Time {
	var soonest time.Time
	for _, account := range accounts {
		if account.CooldownUntil.IsZero() {
			continue
		}
		if soonest.IsZero() || account.CooldownUntil.Before(soonest) {
			soonest = account.CooldownUntil
		}
	}
	return soonest
}

// retryAfterUntil returns the delay from now to until, clamped to at least one second so a
// Retry-After header is always meaningful. A zero until falls back to the default cooldown.
func retryAfterUntil(until, now time.Time) time.Duration {
	if until.IsZero() {
		return defaultCooldown
	}
	if d := until.Sub(now); d > time.Second {
		return d
	}
	return time.Second
}
