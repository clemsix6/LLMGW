package alert

import (
	"sync"
	"sync/atomic"
	"time"
)

// DefaultWindow is the minimum re-notification interval per entity and kind.
const DefaultWindow = 15 * time.Minute

// Tracked entity states. Each observer owns a disjoint subset of them, and the
// healthy one differs per observer, which is why transitionLocked is told.
const (
	stateOK           = "ok"
	stateUnauthorized = "unauthorized"
	stateRateLimited  = "rate_limited"
	stateFailing      = "failing"
	stateBlocked      = "blocked"
	stateWarned       = "warned"
	stateExpiring     = "expiring"
	stateExpired      = "expired"
	stateUp           = "up"
	stateDown         = "down"
)

// Entity key prefixes. The trailing separator is what keeps one project's
// budgets from matching another whose name merely starts the same.
const (
	keyCredentialPrefix = "credential\x00"
	keyBudgetPrefix     = "budget\x00"
	keyProjectKeyPrefix = "key\x00"
	keyGeneration       = "generation"
	keyDatabase         = "database"
)

// entry is the tracked state of one entity.
type entry struct {
	current          string             // current is the state last observed.
	delivered        string             // delivered is the state last accepted by the notifier.
	deliveredKind    Kind               // deliveredKind is the kind last accepted for this entity.
	deliveredHealthy bool               // deliveredHealthy reports whether delivered is the healthy state.
	at               map[Kind]time.Time // at is when each kind was last delivered for this entity.
	fields           []Field            // fields are the identifying fields of the last breach, for clearing events.
}

// Tracker converts observed facts into state transitions.
//
// It is safe for concurrent use, and safe on a nil receiver: every method
// returns immediately, which is what makes wiring nil-tolerant everywhere else.
type Tracker struct {
	notifier Notifier                   // notifier delivers the events the tracker builds.
	labels   map[string]CredentialLabel // labels renders credential IDs as operator-facing names.
	window   time.Duration              // window is the minimum re-notification interval per entity and kind.
	now      func() time.Time           // now supplies the observation clock.

	mu      sync.Mutex        // mu guards entries and the generation counters.
	entries map[string]*entry // entries holds one state per watched entity.

	generationFailures   int // generationFailures counts consecutive failing generations.
	generationLastStatus int // generationLastStatus is the status of the last observed generation.

	databaseDown atomic.Bool // databaseDown reports a down state still to be reconciled.
}

// New builds a tracker delivering to notifier.
//
// A nil notifier makes every observation a no-op, which is how the disabled
// configuration is expressed without branching at any call site. labels renders
// credential IDs and may be nil. A zero window disables anti-flap suppression.
func New(
	notifier Notifier,
	labels map[string]CredentialLabel,
	window time.Duration,
	now func() time.Time,
) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{
		notifier: notifier,
		labels:   labels,
		window:   window,
		now:      now,
		entries:  make(map[string]*entry),
	}
}

// Emit delivers a one-shot event that carries no tracked state.
func (t *Tracker) Emit(kind Kind, fields ...Field) {
	if t.disabled() {
		return
	}
	t.notifier.Notify(t.buildEvent(kind, kind.Title(), fields))
}

// disabled reports whether the tracker can deliver anything at all.
func (t *Tracker) disabled() bool {
	return t == nil || t.notifier == nil
}

// buildEvent assembles one event with the severity its kind fixes.
func (t *Tracker) buildEvent(kind Kind, summary string, fields []Field) Event {
	return Event{
		Kind:     kind,
		Severity: kind.severity(),
		Summary:  summary,
		Fields:   fields,
		At:       t.now().UTC(),
	}
}

// transitionLocked emits when the observed state differs from what was
// delivered, deferring rather than discarding when the anti-flap window is
// still open.
//
// healthy tells it whether state is this entity's healthy sentinel, which
// differs per observer. The caller must hold t.mu, and must keep holding it
// across Notify.
func (t *Tracker) transitionLocked(
	key string,
	state string,
	healthy bool,
	kind Kind,
	summary string,
	fields []Field,
) {
	tracked := t.entryLocked(key)
	tracked.current = state

	if !t.shouldEmitLocked(tracked, state, healthy, kind) {
		return
	}
	if t.notifier.Notify(t.buildEvent(kind, summary, fields)) {
		t.commitLocked(tracked, state, healthy, kind)
	}
}

// entryLocked returns the entity's state, creating it on first observation.
// The caller must hold t.mu.
func (t *Tracker) entryLocked(key string) *entry {
	tracked := t.entries[key]
	if tracked == nil {
		tracked = &entry{at: make(map[Kind]time.Time)}
		t.entries[key] = tracked
	}
	return tracked
}

// shouldEmitLocked applies the transition rules of the transition semantics:
// no change is silent, an entity first seen healthy has not transitioned, and
// the window suppresses everything but an escalation from a degraded state.
// The caller must hold t.mu.
func (t *Tracker) shouldEmitLocked(tracked *entry, state string, healthy bool, kind Kind) bool {
	if state == tracked.delivered {
		return false
	}
	if tracked.delivered == "" && healthy {
		return false
	}
	if t.now().Sub(tracked.at[kind]) >= t.window {
		return true
	}
	return escalates(tracked, kind)
}

// escalates reports whether kind outranks the entity's last delivered kind
// while that entity is degraded, the one case that bypasses the window.
//
// The degraded requirement is what stops a credential flapping between failure
// and recovery from bursting through the window on every failure.
func escalates(tracked *entry, kind Kind) bool {
	if tracked.deliveredHealthy {
		return false
	}
	return severityRank(kind.severity()) > severityRank(tracked.deliveredKind.severity())
}

// commitLocked records a delivered transition, which is what the window and the
// escalation test read. The caller must hold t.mu.
func (t *Tracker) commitLocked(tracked *entry, state string, healthy bool, kind Kind) {
	tracked.delivered = state
	tracked.deliveredKind = kind
	tracked.deliveredHealthy = healthy
	tracked.at[kind] = t.now()
}

// firstNonEmpty returns the preferred value, or the fallback when it is empty.
func firstNonEmpty(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}
