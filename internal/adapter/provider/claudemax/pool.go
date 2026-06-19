package claudemax

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// defaultCooldown is applied on a 429 with no reset hint and on transient upstream failures
	// (5xx / auth). Deliberately short (never clewdr's 1h) so a transient limit clears quickly and
	// the account returns to the pool.
	defaultCooldown = 60 * time.Second

	// usageExhaustedCooldown benches an account that reports "out of extra usage". Empirically this
	// is a short throttle on the subscription's programmatic path, not a real budget exhaustion: the
	// account serves again within ~1s. So the bench is tiny — both to retry the account quickly and
	// to keep the all-cooling 503's Retry-After short, since clients (Hermes' SDK) honor it.
	usageExhaustedCooldown = 5 * time.Second

	// deadTokenCooldown benches an account whose credential can't be refreshed (a dead session key
	// needing an operator re-seed); cooling it longer avoids hammering the OAuth bootstrap each Send.
	deadTokenCooldown = 15 * time.Minute
)

// AllCoolingError reports that every account in the pool is on cooldown, so no request can be
// served right now. After is the delay until the soonest account becomes available; the
// handler maps it to a 503 with a Retry-After header.
type AllCoolingError struct {
	After time.Duration // After is the wait until the earliest account's cooldown clears.
}

// Error implements the error interface.
func (e *AllCoolingError) Error() string {
	return fmt.Sprintf("all accounts cooling; retry after %s", e.After)
}

// HTTPStatus returns 503 Service Unavailable; all accounts are cooling.
func (e *AllCoolingError) HTTPStatus() int { return 503 }

// ErrorType returns the stable classifier "all_cooling".
func (e *AllCoolingError) ErrorType() string { return "all_cooling" }

// RetryAfter returns the known backoff duration (always present for AllCoolingError).
func (e *AllCoolingError) RetryAfter() (time.Duration, bool) { return e.After, true }

// accountStore is the persistence the multi-account provider needs: per-account token access (for
// the token manager) plus the cooldown state that drives round-robin selection.
type accountStore interface {
	tokenStore

	// LoadAccounts returns every account with its cooldown state, ordered by label.
	LoadAccounts(ctx context.Context) ([]domain.Account, error)

	// SetCooldown records that an account is rate-limited until the given time.
	SetCooldown(ctx context.Context, account string, until time.Time) error
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
	if err := p.store.SetCooldown(ctx, account, until); err != nil {
		log.Printf("llmgw: set cooldown for %q: %v", account, err)
	}
}

// allCooling builds the AllCoolingError returned when no account could serve the request: it
// re-reads the pool and sets RetryAfter to the delay until the soonest cooldown clears, falling
// back to the default when the pool can't be read or carries no cooldown.
func (p *Provider) allCooling(ctx context.Context, now time.Time) error {
	accounts, err := p.store.LoadAccounts(ctx)
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
