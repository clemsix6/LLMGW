package alert

import (
	"strconv"
	"strings"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// ObserveAdmission records the budget breaches of one generation admission.
//
// Clearing is the absence of a breach, so every tracked budget of that project
// missing from both slices transitions back to healthy.
func (t *Tracker) ObserveAdmission(project string, blocks, warnings []governance.BudgetBreach) {
	if t.disabled() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	breached := make(map[string]struct{}, len(blocks)+len(warnings))
	t.observeBreachesLocked(project, blocks, stateBlocked, KindBudgetBlocked, breached)
	t.observeBreachesLocked(project, warnings, stateWarned, KindBudgetWarning, breached)
	t.clearBudgetsLocked(project, breached)
}

// observeBreachesLocked transitions every breached budget entity of one
// project, remembering the fields its later clearing event needs.
// The caller must hold t.mu.
func (t *Tracker) observeBreachesLocked(
	project string,
	breaches []governance.BudgetBreach,
	state string,
	kind Kind,
	breached map[string]struct{},
) {
	for _, breach := range breaches {
		key := budgetKey(project, breach.Limit)
		breached[key] = struct{}{}

		identity := budgetIdentity(project, breach.Limit)
		t.entryLocked(key).fields = identity
		t.transitionLocked(key, state, false, kind, kind.Title(), breachFields(identity, breach.ResetAt))
	}
}

// clearBudgetsLocked clears every tracked budget of one project the admission
// no longer breaches. The caller must hold t.mu.
func (t *Tracker) clearBudgetsLocked(project string, breached map[string]struct{}) {
	prefix := keyBudgetPrefix + project + "\x00"

	for key, tracked := range t.entries {
		if _, found := breached[key]; found {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		t.transitionLocked(key, stateOK, true, KindBudgetCleared, KindBudgetCleared.Title(), tracked.fields)
	}
}

// budgetKey identifies one budget entity, matching the uniqueness of a budget
// limit so a warning and a block on the same dimension stay separate.
func budgetKey(project string, limit governance.BudgetLimit) string {
	return keyBudgetPrefix + project + "\x00" +
		string(limit.Dimension) + "\x00" +
		string(limit.Window) + "\x00" +
		string(limit.Action)
}

// budgetIdentity renders what identifies a budget whether or not it is
// currently breached, which is exactly what a clearing event can still carry.
func budgetIdentity(project string, limit governance.BudgetLimit) []Field {
	return []Field{
		{Name: "Project", Value: project},
		{Name: "Dimension", Value: string(limit.Dimension)},
		{Name: "Window", Value: string(limit.Window)},
		{Name: "Limit", Value: strconv.FormatFloat(limit.MaxValue, 'f', -1, 64)},
	}
}

// breachFields adds the reset time a breach carries to its identity, without
// aliasing the slice the entity keeps for its clearing event.
func breachFields(identity []Field, resetAt time.Time) []Field {
	fields := make([]Field, 0, len(identity)+1)
	fields = append(fields, identity...)
	return append(fields, Field{Name: "Resets at", Value: resetAt.UTC().Format(time.RFC3339)})
}
