package alert_test

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// attempt is one upstream attempt fed to ObserveAttempt.
type attempt struct {
	credentialID string // credentialID identifies the provider credential.
	model        string // model is the model the attempt targeted.
	failed       bool   // failed reports whether the attempt failed.
	status       int    // status is the upstream HTTP status, zero when there was none.
}

func TestCredentialTransitions(t *testing.T) {
	cases := []struct {
		name     string
		attempts []attempt
		want     []alert.Kind
	}{
		{
			name:     "a repeat emits nothing",
			attempts: []attempt{{"cred-1", "opus", true, 429}, {"cred-1", "opus", true, 429}},
			want:     []alert.Kind{alert.KindCredentialRateLimited},
		},
		{
			name:     "the same credential on another model is another entity",
			attempts: []attempt{{"cred-1", "opus", true, 429}, {"cred-1", "sonnet", true, 429}},
			want:     []alert.Kind{alert.KindCredentialRateLimited, alert.KindCredentialRateLimited},
		},
		{
			name:     "a first success is not a recovery",
			attempts: []attempt{{"cred-1", "opus", false, 0}},
			want:     nil,
		},
		{
			name:     "a success after a delivered degraded state recovers",
			attempts: []attempt{{"cred-1", "opus", true, 429}, {"cred-1", "opus", false, 0}},
			want:     []alert.Kind{alert.KindCredentialRateLimited, alert.KindCredentialRecovered},
		},
		{
			name:     "recovery is scoped to the model that degraded",
			attempts: []attempt{{"cred-1", "opus", true, 429}, {"cred-1", "sonnet", false, 0}},
			want:     []alert.Kind{alert.KindCredentialRateLimited},
		},
		{
			name: "client 4xx statuses leave the state untouched",
			attempts: []attempt{
				{"cred-1", "opus", true, 429},
				{"cred-1", "opus", true, 400},
				{"cred-1", "opus", true, 404},
				{"cred-1", "opus", true, 422},
				{"cred-1", "opus", false, 0},
			},
			want: []alert.Kind{alert.KindCredentialRateLimited, alert.KindCredentialRecovered},
		},
		{
			name:     "401 and 403 are unauthorized",
			attempts: []attempt{{"cred-1", "opus", true, 401}, {"cred-2", "opus", true, 403}},
			want:     []alert.Kind{alert.KindCredentialUnauthorized, alert.KindCredentialUnauthorized},
		},
		{
			name:     "a 5xx is a failing credential",
			attempts: []attempt{{"cred-1", "opus", true, 503}},
			want:     []alert.Kind{alert.KindCredentialFailing},
		},
		{
			name:     "a statusless transport failure is a failing credential",
			attempts: []attempt{{"cred-1", "opus", true, 0}},
			want:     []alert.Kind{alert.KindCredentialFailing},
		},
		{
			name:     "an attempt without a credential is ignored",
			attempts: []attempt{{"", "opus", true, 429}},
			want:     nil,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sink := newNotifier()
			tracker := newTracker(sink, newClock())

			for _, one := range testCase.attempts {
				tracker.ObserveAttempt("claude", one.credentialID, one.model, one.failed, one.status)
			}

			assertKinds(t, sink, testCase.want...)
		})
	}
}

// TestCredentialLabelRendering pins the operator-facing identity: a known
// credential renders its label, an unknown one falls back to its ID.
func TestCredentialLabelRendering(t *testing.T) {
	sink := newNotifier()
	labels := map[string]alert.CredentialLabel{
		"cred-1": {Provider: "claude", Label: "ops@example.com"},
	}
	tracker := alert.New(sink, labels, alert.DefaultWindow, newClock().Now)

	tracker.ObserveAttempt("", "cred-1", "opus", true, 401)
	tracker.ObserveAttempt("codex", "cred-2", "opus", true, 401)

	known := eventAt(t, sink, 0)
	if got := fieldValue(known, "Credential"); got != "ops@example.com" {
		t.Fatalf("credential = %q, want the label", got)
	}
	if got := fieldValue(known, "Provider"); got != "claude" {
		t.Fatalf("provider = %q, want the label's provider", got)
	}

	unknown := eventAt(t, sink, 1)
	if got := fieldValue(unknown, "Credential"); got != "cred-2" {
		t.Fatalf("credential = %q, want the identifier", got)
	}
	if got := fieldValue(unknown, "Provider"); got != "codex" {
		t.Fatalf("provider = %q, want the observed provider", got)
	}
}

// TestCredentialFieldNames pins the exact field set of every credential kind,
// including the two cases where the upstream status must not be rendered.
func TestCredentialFieldNames(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)
	tracker.ObserveAttempt("claude", "cred-1", "opus", false, 429)
	tracker.ObserveAttempt("claude", "cred-2", "opus", true, 0)

	assertKinds(t,
		sink,
		alert.KindCredentialRateLimited,
		alert.KindCredentialRecovered,
		alert.KindCredentialFailing,
	)

	limited := eventAt(t, sink, 0)
	assertFieldNames(t, limited, "Provider", "Credential", "Model", "Status")
	if got := fieldValue(limited, "Status"); got != "429" {
		t.Fatalf("status = %q, want 429", got)
	}

	// A success can carry a residual status from a previous try, which must
	// never be rendered beside a recovery.
	assertFieldNames(t, eventAt(t, sink, 1), "Provider", "Credential", "Model")

	// A transport failure has no status at all: the field is omitted and the
	// summary says so rather than rendering a zero.
	failing := eventAt(t, sink, 2)
	assertFieldNames(t, failing, "Provider", "Credential", "Model")

	tracker.ObserveAttempt("claude", "cred-3", "opus", true, 503)
	if statused := eventAt(t, sink, 3); statused.Summary == failing.Summary {
		t.Fatalf("statusless summary = %q, want it to differ from the statused one", failing.Summary)
	}
}

// TestWindowIsScopedPerEntityAndKind pins that the anti-flap window is held per
// (entity, kind) rather than per entity: both kinds here are warnings, so no
// escalation can explain the second event.
func TestWindowIsScopedPerEntityAndKind(t *testing.T) {
	sink := newNotifier()
	clock := newClock()
	tracker := newTracker(sink, clock)

	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)
	clock.Advance(time.Minute)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 503)

	assertKinds(t, sink, alert.KindCredentialRateLimited, alert.KindCredentialFailing)
}

// TestEscalationPassesTheWindowButDeEscalationDoesNot pins the direction of the
// escalation rule, on an entity whose window for both kinds is already open.
func TestEscalationPassesTheWindowButDeEscalationDoesNot(t *testing.T) {
	sink := newNotifier()
	clock := newClock()
	tracker := newTracker(sink, clock)

	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 401)
	clock.Advance(time.Minute)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)

	assertKinds(t, sink, alert.KindCredentialUnauthorized, alert.KindCredentialRateLimited)

	// Warning to critical, inside the window of a kind already delivered.
	clock.Advance(time.Minute)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 401)

	assertKinds(t,
		sink,
		alert.KindCredentialUnauthorized,
		alert.KindCredentialRateLimited,
		alert.KindCredentialUnauthorized,
	)

	// Critical back down to warning, inside the window: suppressed.
	clock.Advance(time.Minute)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)

	assertKinds(t,
		sink,
		alert.KindCredentialUnauthorized,
		alert.KindCredentialRateLimited,
		alert.KindCredentialUnauthorized,
	)
}
