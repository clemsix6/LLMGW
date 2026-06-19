package codex

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// poolNow is a fixed reference time for the pure selection/cooldown tests.
var poolNow = time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

// TestSelectOrderSkipsCoolingAccounts proves a cooling account is excluded from selection.
func TestSelectOrderSkipsCoolingAccounts(t *testing.T) {
	p := &Provider{}
	now := time.Now()
	accounts := []domain.Account{{Label: "a", CooldownUntil: now.Add(time.Minute)}, {Label: "b"}}

	order := p.selectOrder(accounts, now)

	if len(order) != 1 || order[0] != "b" {
		t.Fatalf("selectOrder = %v, want [b]", order)
	}
}

// TestSelectOrderIncludesAccountAfterCooldownExpires proves an account rejoins selection once its
// cooldown lies in the past.
func TestSelectOrderIncludesAccountAfterCooldownExpires(t *testing.T) {
	accounts := []domain.Account{
		{Label: "a", CooldownUntil: poolNow.Add(-time.Minute)}, // expired cooldown: available again
		{Label: "b"},
	}

	order := (&Provider{}).selectOrder(accounts, poolNow)

	if len(order) != 2 {
		t.Fatalf("selectOrder = %v, want both accounts (a's cooldown expired)", order)
	}
}

// TestSelectOrderRotatesRoundRobin proves the cursor advances once per call so consecutive Sends
// start from a different account.
func TestSelectOrderRotatesRoundRobin(t *testing.T) {
	accounts := []domain.Account{{Label: "a"}, {Label: "b"}, {Label: "c"}}

	p := &Provider{}
	wantFirst := []string{"a", "b", "c", "a"}
	for call, want := range wantFirst {
		order := p.selectOrder(accounts, poolNow)
		if len(order) != 3 || order[0] != want {
			t.Fatalf("call %d: selectOrder = %v, want first=%q", call, order, want)
		}
	}
}

// TestSelectOrderEmptyWhenAllCooling proves selection yields nothing when every account is cooling.
func TestSelectOrderEmptyWhenAllCooling(t *testing.T) {
	accounts := []domain.Account{
		{Label: "a", CooldownUntil: poolNow.Add(time.Minute)},
		{Label: "b", CooldownUntil: poolNow.Add(time.Hour)},
	}

	if order := (&Provider{}).selectOrder(accounts, poolNow); len(order) != 0 {
		t.Fatalf("selectOrder = %v, want empty (all cooling)", order)
	}
}

// TestCooling proves an account is cooling only while a non-zero cooldown lies in the future.
func TestCooling(t *testing.T) {
	cases := []struct {
		name    string
		account domain.Account
		want    bool
	}{
		{"never set", domain.Account{Label: "a"}, false},
		{"future", domain.Account{Label: "a", CooldownUntil: poolNow.Add(time.Minute)}, true},
		{"past", domain.Account{Label: "a", CooldownUntil: poolNow.Add(-time.Minute)}, false},
	}

	for _, c := range cases {
		if got := cooling(c.account, poolNow); got != c.want {
			t.Errorf("cooling(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSoonestCooldown proves the earliest non-zero cooldown wins, ignoring un-cooled accounts.
func TestSoonestCooldown(t *testing.T) {
	soon := poolNow.Add(2 * time.Minute)
	accounts := []domain.Account{
		{Label: "a", CooldownUntil: poolNow.Add(5 * time.Minute)},
		{Label: "b", CooldownUntil: soon},
		{Label: "c"}, // never cooled: ignored
	}

	if got := soonestCooldown(accounts); !got.Equal(soon) {
		t.Fatalf("soonestCooldown = %v, want %v", got, soon)
	}
	if got := soonestCooldown([]domain.Account{{Label: "a"}}); !got.IsZero() {
		t.Fatalf("soonestCooldown(no cooldowns) = %v, want zero", got)
	}
}

// TestRetryAfterUntil proves the delay is clamped to at least a second and falls back to the
// default cooldown for a zero until.
func TestRetryAfterUntil(t *testing.T) {
	if got := retryAfterUntil(time.Time{}, poolNow); got != defaultCooldown {
		t.Errorf("retryAfterUntil(zero) = %v, want default %v", got, defaultCooldown)
	}
	if got := retryAfterUntil(poolNow.Add(-time.Minute), poolNow); got != time.Second {
		t.Errorf("retryAfterUntil(past) = %v, want 1s (clamped)", got)
	}
	if got := retryAfterUntil(poolNow.Add(2*time.Minute), poolNow); got != 2*time.Minute {
		t.Errorf("retryAfterUntil(+2m) = %v, want 2m", got)
	}
}

// TestCooldownForRateLimit proves a RateLimitError with a known reset time returns that time.
func TestCooldownForRateLimit(t *testing.T) {
	resetAt := poolNow.Add(5 * time.Minute)
	until, retry := cooldownFor(&RateLimitError{ResetAt: resetAt}, poolNow)
	if !retry {
		t.Fatal("cooldownFor(RateLimitError with reset) should retry")
	}
	if !until.Equal(resetAt) {
		t.Errorf("until = %v, want %v", until, resetAt)
	}
}

// TestCooldownForRateLimitNoReset proves a RateLimitError without a reset time falls back to
// the default cooldown.
func TestCooldownForRateLimitNoReset(t *testing.T) {
	until, retry := cooldownFor(&RateLimitError{}, poolNow)
	if !retry {
		t.Fatal("cooldownFor(RateLimitError no reset) should retry")
	}
	if until != poolNow.Add(defaultCooldown) {
		t.Errorf("until = %v, want now+defaultCooldown", until)
	}
}

// TestCooldownForDeadToken proves a DeadRefreshTokenError is cooled and failed over.
func TestCooldownForDeadToken(t *testing.T) {
	until, retry := cooldownFor(&DeadRefreshTokenError{Account: "x"}, poolNow)
	if !retry {
		t.Fatal("cooldownFor(DeadRefreshTokenError) should retry")
	}
	if until != poolNow.Add(deadTokenCooldown) {
		t.Errorf("until = %v, want now+deadTokenCooldown", until)
	}
}

// TestCooldownForUpstreamFailover proves 401/403/5xx upstream errors trigger failover.
func TestCooldownForUpstreamFailover(t *testing.T) {
	for _, status := range []int{401, 403, 500, 502, 503} {
		_, retry := cooldownFor(&UpstreamError{Status: status}, poolNow)
		if !retry {
			t.Errorf("cooldownFor(UpstreamError %d) should retry (failover status)", status)
		}
	}
}

// TestCooldownForUpstreamNoFailover proves other 4xx upstream errors surface without failover.
func TestCooldownForUpstreamNoFailover(t *testing.T) {
	for _, status := range []int{400, 404, 422} {
		_, retry := cooldownFor(&UpstreamError{Status: status}, poolNow)
		if retry {
			t.Errorf("cooldownFor(UpstreamError %d) should NOT retry (request-level error)", status)
		}
	}
}
