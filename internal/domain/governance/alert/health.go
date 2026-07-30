package alert

import "strconv"

// generationFailureThreshold is how many consecutive failing generations are
// needed before the outage is reported, so an isolated hiccup stays quiet.
const generationFailureThreshold = 3

// ObserveGeneration records the final status of one admitted generation.
func (t *Tracker) ObserveGeneration(status int) {
	if t.disabled() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.generationLastStatus = status
	if status >= 500 {
		t.observeGenerationFailureLocked()
		return
	}

	t.generationFailures = 0
	t.transitionLocked(
		keyGeneration,
		stateOK,
		true,
		KindGenerationRecovered,
		KindGenerationRecovered.Title(),
		t.generationFieldsLocked(),
	)
}

// observeGenerationFailureLocked counts one failing generation and transitions
// only once the threshold is reached: below it the state is left untouched, so
// a failure is never mistaken for a success. The caller must hold t.mu.
func (t *Tracker) observeGenerationFailureLocked() {
	t.generationFailures++
	if t.generationFailures < generationFailureThreshold {
		return
	}

	t.transitionLocked(
		keyGeneration,
		stateFailing,
		false,
		KindGenerationFailures,
		KindGenerationFailures.Title(),
		t.generationFieldsLocked(),
	)
}

// generationFieldsLocked renders the consecutive failure count and the last
// observed status. The caller must hold t.mu.
func (t *Tracker) generationFieldsLocked() []Field {
	return []Field{
		{Name: "Consecutive failures", Value: strconv.Itoa(t.generationFailures)},
		{Name: "Last status", Value: strconv.Itoa(t.generationLastStatus)},
	}
}

// ObserveDatabase records one database outcome from any repository call site.
func (t *Tracker) ObserveDatabase(healthy bool) {
	if t.disabled() {
		return
	}

	if healthy {
		t.observeDatabaseUp()
		return
	}
	t.observeDatabaseDown()
}

// observeDatabaseDown raises the pending flag before reporting the outage, so a
// report that never reaches the notifier still leaves a restore owed.
//
// The flag is raised under the lock so a concurrent success can never observe
// it while this outage is still unapplied, lower it, and strand the entity down
// with the flag clear.
func (t *Tracker) observeDatabaseDown() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.databaseDown.Store(true)
	t.transitionLocked(
		keyDatabase,
		stateDown,
		false,
		KindDatabaseUnavailable,
		KindDatabaseUnavailable.Title(),
		nil,
	)
}

// observeDatabaseUp is the hot path: it takes no lock while no outage is
// pending, and lowers the flag only once no outage is owed any more.
//
// The flag means "a down state is still to be reconciled", so it survives a
// restore the notifier suppressed or dropped — delivered is then still down and
// the next success retries. It is lowered when the outage itself was never
// delivered, because nothing is owed and the lock-free path can resume.
func (t *Tracker) observeDatabaseUp() {
	if !t.databaseDown.Load() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.transitionLocked(
		keyDatabase,
		stateUp,
		true,
		KindDatabaseRestored,
		KindDatabaseRestored.Title(),
		nil,
	)
	if t.entryLocked(keyDatabase).delivered != stateDown {
		t.databaseDown.Store(false)
	}
}
