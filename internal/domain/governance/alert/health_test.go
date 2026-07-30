package alert_test

import (
	"testing"

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
