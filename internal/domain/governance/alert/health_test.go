package alert_test

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// TestGenerationFailuresNeedThreeConsecutive pins the threshold: two failures
// stay quiet, the third reports, and a fourth adds no second message.
func TestGenerationFailuresNeedThreeConsecutive(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(502)

	assertKinds(t, sink)

	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(500)

	assertKinds(t, sink, alert.KindGenerationFailures)
}

// TestGenerationRecoveryResetsTheCounter pins that a served generation both
// reports the recovery and restarts the count from zero.
func TestGenerationRecoveryResetsTheCounter(t *testing.T) {
	sink := newNotifier()
	clock := newClock()
	tracker := newTracker(sink, clock)

	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(200)

	assertKinds(t, sink, alert.KindGenerationFailures, alert.KindGenerationRecovered)

	// Past the window, so only the counter can keep the next two quiet.
	clock.Advance(alert.DefaultWindow)
	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)

	assertKinds(t, sink, alert.KindGenerationFailures, alert.KindGenerationRecovered)

	tracker.ObserveGeneration(503)

	assertKinds(t,
		sink,
		alert.KindGenerationFailures,
		alert.KindGenerationRecovered,
		alert.KindGenerationFailures,
	)
}

// TestDatabaseTransitions pins the hot-path guard: a healthy observation on a
// healthy tracker reports nothing, and a restore is announced exactly once.
func TestDatabaseTransitions(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveDatabase(true)

	assertKinds(t, sink)

	tracker.ObserveDatabase(false)
	tracker.ObserveDatabase(false)

	assertKinds(t, sink, alert.KindDatabaseUnavailable)

	tracker.ObserveDatabase(true)
	tracker.ObserveDatabase(true)

	assertKinds(t, sink, alert.KindDatabaseUnavailable, alert.KindDatabaseRestored)
}

// TestRejectedDatabaseRestoreIsRetried pins that the lock-free hot path never
// costs an alert: a restore the notifier refuses leaves the outage owed, so the
// next success offers it again instead of returning at the guard.
func TestRejectedDatabaseRestoreIsRetried(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveDatabase(false)

	sink.setAccepted(false)
	tracker.ObserveDatabase(true)

	sink.setAccepted(true)
	tracker.ObserveDatabase(true)

	assertKinds(t,
		sink,
		alert.KindDatabaseUnavailable,
		alert.KindDatabaseRestored,
		alert.KindDatabaseRestored,
	)
}

// TestSuppressedDatabaseRestoreIsDeferred pins the same guarantee for the other
// way a restore is withheld: the anti-flap window defers it, and the outage
// stays owed until a later success carries it past the window.
func TestSuppressedDatabaseRestoreIsDeferred(t *testing.T) {
	sink := newNotifier()
	clock := newClock()
	tracker := newTracker(sink, clock)

	tracker.ObserveDatabase(false)

	clock.Advance(alert.DefaultWindow + time.Minute)
	tracker.ObserveDatabase(true)

	clock.Advance(time.Minute)
	tracker.ObserveDatabase(false)

	// Inside the restore kind's window, and a de-escalation, so it is withheld.
	clock.Advance(time.Minute)
	tracker.ObserveDatabase(true)

	assertKinds(t,
		sink,
		alert.KindDatabaseUnavailable,
		alert.KindDatabaseRestored,
		alert.KindDatabaseUnavailable,
	)

	clock.Advance(alert.DefaultWindow)
	tracker.ObserveDatabase(true)

	assertKinds(t,
		sink,
		alert.KindDatabaseUnavailable,
		alert.KindDatabaseRestored,
		alert.KindDatabaseUnavailable,
		alert.KindDatabaseRestored,
	)
}

// TestHealthFieldNames pins the exact field set of the generation and database
// kinds. Database events carry no field at all: there is nothing to identify
// beyond the transition itself.
func TestHealthFieldNames(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(200)
	tracker.ObserveDatabase(false)
	tracker.ObserveDatabase(true)

	assertKinds(t,
		sink,
		alert.KindGenerationFailures,
		alert.KindGenerationRecovered,
		alert.KindDatabaseUnavailable,
		alert.KindDatabaseRestored,
	)

	failures := eventAt(t, sink, 0)
	assertFieldNames(t, failures, "Consecutive failures", "Last status")
	if got := fieldValue(failures, "Consecutive failures"); got != "3" {
		t.Fatalf("consecutive failures = %q, want 3", got)
	}
	if got := fieldValue(failures, "Last status"); got != "503" {
		t.Fatalf("last status = %q, want 503", got)
	}

	recovered := eventAt(t, sink, 1)
	assertFieldNames(t, recovered, "Consecutive failures", "Last status")
	if got := fieldValue(recovered, "Last status"); got != "200" {
		t.Fatalf("last status = %q, want 200", got)
	}

	assertFieldNames(t, eventAt(t, sink, 2))
	assertFieldNames(t, eventAt(t, sink, 3))
}

// TestHealthSeverities pins the severity every health kind is rendered with,
// since the colour an operator sees is derived from it alone.
func TestHealthSeverities(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)
	tracker.ObserveGeneration(503)
	tracker.ObserveDatabase(false)

	if got := eventAt(t, sink, 0).Severity; got != alert.SeverityCritical {
		t.Fatalf("generation_failures severity = %q, want %q", got, alert.SeverityCritical)
	}
	if got := eventAt(t, sink, 1).Severity; got != alert.SeverityCritical {
		t.Fatalf("database_unavailable severity = %q, want %q", got, alert.SeverityCritical)
	}
}

// TestExhaustedPoolCountsAsAGenerationFailure pins the 429: the SDK answers an
// exhausted credential pool that way, so treating it as a served generation
// would hide the outage the critical signal exists to catch.
func TestExhaustedPoolCountsAsAGenerationFailure(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(429)
	tracker.ObserveGeneration(429)

	assertKinds(t, sink)

	tracker.ObserveGeneration(429)

	assertKinds(t, sink, alert.KindGenerationFailures)
}

// TestExhaustedPoolNeverAnnouncesARecovery pins the misleading all-clear: from a
// reported outage, a 429 must not reset the state, because no client is served.
func TestExhaustedPoolNeverAnnouncesARecovery(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(429)

	assertKinds(t, sink, alert.KindGenerationFailures)
}

// TestClientErrorNeverClearsAReportedOutage pins half the exclusion: one
// project's malformed request must not post the all-clear while the gateway is
// still failing everyone else. A fresh tracker cannot prove this — it stays
// silent whatever the classification — so the outage is reported first.
func TestClientErrorNeverClearsAReportedOutage(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(400)

	assertKinds(t, sink, alert.KindGenerationFailures)
}

// TestClientErrorNeverHidesAnOutage pins the other half: a client 4xx must not
// reset the consecutive count, or one misbehaving project could keep an outage
// permanently unreported.
func TestClientErrorNeverHidesAnOutage(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(500)
	tracker.ObserveGeneration(404)
	tracker.ObserveGeneration(500)

	assertKinds(t, sink, alert.KindGenerationFailures)
}
