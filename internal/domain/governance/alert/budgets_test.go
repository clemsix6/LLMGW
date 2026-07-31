package alert_test

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// budgetResetAt is the reset time every seeded breach carries.
var budgetResetAt = fixedNow.Add(time.Hour)

// budgetBreach builds one breached limit for the tests.
func budgetBreach(
	dimension governance.Dimension,
	window governance.Window,
	action governance.Action,
	maxValue float64,
) governance.BudgetBreach {
	return governance.BudgetBreach{
		Limit: governance.BudgetLimit{
			Dimension: dimension,
			Window:    window,
			MaxValue:  maxValue,
			Action:    action,
		},
		ResetAt: budgetResetAt,
	}
}

// TestBudgetWarnAndBlockAreSeparateEntities pins the entity key: a warn and a
// block rule on the same dimension and window cannot overwrite one another, and
// each clears on its own.
func TestBudgetWarnAndBlockAreSeparateEntities(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	block := budgetBreach(governance.DimensionCalls, governance.WindowHour, governance.ActionBlock, 100)
	warn := budgetBreach(governance.DimensionCalls, governance.WindowHour, governance.ActionWarn, 80)
	blocks := []governance.BudgetBreach{block}
	warnings := []governance.BudgetBreach{warn}

	tracker.ObserveAdmission("alpha", blocks, warnings)
	tracker.ObserveAdmission("alpha", blocks, warnings)

	assertKinds(t, sink, alert.KindBudgetBlocked, alert.KindBudgetWarning)

	// The block clears while the warning is still breached.
	tracker.ObserveAdmission("alpha", nil, warnings)

	assertKinds(t, sink, alert.KindBudgetBlocked, alert.KindBudgetWarning, alert.KindBudgetCleared)
	if got := fieldValue(eventAt(t, sink, 2), "Limit"); got != "100" {
		t.Fatalf("cleared limit = %q, want the block's", got)
	}

	// Then the warning clears too, and the block stays quiet.
	tracker.ObserveAdmission("alpha", nil, nil)

	assertKinds(t,
		sink,
		alert.KindBudgetBlocked,
		alert.KindBudgetWarning,
		alert.KindBudgetCleared,
		alert.KindBudgetCleared,
	)
	if got := fieldValue(eventAt(t, sink, 3), "Limit"); got != "80" {
		t.Fatalf("cleared limit = %q, want the warning's", got)
	}
}

// TestBudgetClearedOnlyFromADeliveredBreach pins that a breach the notifier
// never accepted produces no clearing event, since nothing was ever announced.
func TestBudgetClearedOnlyFromADeliveredBreach(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	block := budgetBreach(governance.DimensionCalls, governance.WindowHour, governance.ActionBlock, 100)

	sink.setAccepted(false)
	tracker.ObserveAdmission("alpha", []governance.BudgetBreach{block}, nil)

	sink.setAccepted(true)
	tracker.ObserveAdmission("alpha", nil, nil)

	assertKinds(t, sink, alert.KindBudgetBlocked)
}

// TestBudgetClearIsScopedToOneProject pins that clearing scans one project's
// entities only. The second project's name starts with the first, so a scan
// missing the separator would clear it too.
func TestBudgetClearIsScopedToOneProject(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	block := []governance.BudgetBreach{
		budgetBreach(governance.DimensionCalls, governance.WindowHour, governance.ActionBlock, 100),
	}

	tracker.ObserveAdmission("alpha", block, nil)
	tracker.ObserveAdmission("alpha-secondary", block, nil)

	assertKinds(t, sink, alert.KindBudgetBlocked, alert.KindBudgetBlocked)

	tracker.ObserveAdmission("alpha", nil, nil)

	assertKinds(t, sink, alert.KindBudgetBlocked, alert.KindBudgetBlocked, alert.KindBudgetCleared)
	if got := fieldValue(eventAt(t, sink, 2), "Project"); got != "alpha" {
		t.Fatalf("cleared project = %q, want alpha", got)
	}

	// The other project is still blocked, which is what its own clear proves.
	tracker.ObserveAdmission("alpha-secondary", nil, nil)

	assertKinds(t,
		sink,
		alert.KindBudgetBlocked,
		alert.KindBudgetBlocked,
		alert.KindBudgetCleared,
		alert.KindBudgetCleared,
	)
	if got := fieldValue(eventAt(t, sink, 3), "Project"); got != "alpha-secondary" {
		t.Fatalf("cleared project = %q, want alpha-secondary", got)
	}
}

// TestBudgetFieldNames pins the exact field set of every budget kind. A clear is
// triggered by a breach's absence, so it carries the identity without the reset
// time no remaining breach could supply.
func TestBudgetFieldNames(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	block := budgetBreach(governance.DimensionCost, governance.WindowDay, governance.ActionBlock, 12.5)
	warn := budgetBreach(governance.DimensionCost, governance.WindowDay, governance.ActionWarn, 10)

	tracker.ObserveAdmission("alpha", []governance.BudgetBreach{block}, []governance.BudgetBreach{warn})
	tracker.ObserveAdmission("alpha", nil, nil)

	blocked := eventAt(t, sink, 0)
	assertFieldNames(t, blocked, "Project", "Dimension", "Window", "Limit", "Resets at")
	assertFieldNames(t, eventAt(t, sink, 1), "Project", "Dimension", "Window", "Limit", "Resets at")

	if got := fieldValue(blocked, "Dimension"); got != string(governance.DimensionCost) {
		t.Fatalf("dimension = %q, want %q", got, governance.DimensionCost)
	}
	if got := fieldValue(blocked, "Window"); got != string(governance.WindowDay) {
		t.Fatalf("window = %q, want %q", got, governance.WindowDay)
	}
	if got := fieldValue(blocked, "Resets at"); got != budgetResetAt.Format(time.RFC3339) {
		t.Fatalf("resets at = %q, want %q", got, budgetResetAt.Format(time.RFC3339))
	}

	assertFieldNames(t, eventAt(t, sink, 2), "Project", "Dimension", "Window", "Limit")
	assertFieldNames(t, eventAt(t, sink, 3), "Project", "Dimension", "Window", "Limit")
}
