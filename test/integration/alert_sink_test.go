package integration

import (
	"sync"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// alertPollInterval is how often Wait re-reads the collected events.
const alertPollInterval = 20 * time.Millisecond

// AlertSink is the in-process alert.Notifier the integration harness delivers to.
//
// The tracker holds its own mutex across Notify, so Notify only appends behind
// this sink's mutex: it never blocks and never calls back into the tracker.
// What this suite must prove is that the real request path produces the right
// events, which is exactly what a sink observes; the HTTP transport has its own
// exhaustive adapter coverage and would only add its production inter-delivery
// throttle between assertions.
type AlertSink struct {
	mu     sync.Mutex    // mu protects events.
	events []alert.Event // events retains every accepted event in delivery order.
}

// Notify records one event and always accepts it.
func (a *AlertSink) Notify(event alert.Event) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.events = append(a.events, event)
	return true
}

// Mark returns the sink's current length so a test can scope its assertions to
// the events it caused.
//
// One tracker and one sink serve the whole package, so an unscoped assertion
// would silently depend on which other file ran first.
func (a *AlertSink) Mark() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.events)
}

// Wait returns the first event at or after from that has this kind and carries
// every field in match, or false on timeout. An empty match matches on kind alone.
func (a *AlertSink) Wait(
	from int,
	kind alert.Kind,
	match []alert.Field,
	timeout time.Duration,
) (alert.Event, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, event := range a.snapshot(from) {
			if eventMatches(event, kind, match) {
				return event, true
			}
		}
		if !time.Now().Before(deadline) {
			return alert.Event{}, false
		}
		time.Sleep(alertPollInterval)
	}
}

// CountFor counts the events at or after from that have this kind and carry
// every field in match.
//
// It is what makes "no second message" assertable; Wait alone cannot express an
// absence.
func (a *AlertSink) CountFor(from int, kind alert.Kind, match []alert.Field) int {
	count := 0
	for _, event := range a.snapshot(from) {
		if eventMatches(event, kind, match) {
			count++
		}
	}
	return count
}

// snapshot copies the events recorded at or after from, so no assertion ever
// reads the slice the tracker is still appending to.
func (a *AlertSink) snapshot(from int) []alert.Event {
	a.mu.Lock()
	defer a.mu.Unlock()

	if from < 0 || from > len(a.events) {
		from = len(a.events)
	}
	return append([]alert.Event(nil), a.events[from:]...)
}

// eventMatches reports whether one event has this kind and carries every field
// in match.
func eventMatches(event alert.Event, kind alert.Kind, match []alert.Field) bool {
	if event.Kind != kind {
		return false
	}
	for _, wanted := range match {
		if !hasField(event.Fields, wanted) {
			return false
		}
	}
	return true
}

// hasField reports whether fields carry wanted's name with wanted's value.
func hasField(fields []alert.Field, wanted alert.Field) bool {
	for _, field := range fields {
		if field == wanted {
			return true
		}
	}
	return false
}

// newAlertTracker builds the one tracker every integration assertion reads.
//
// The labels map is an explicit nil: the harness's credentials are synthesized
// from the YAML codex-api-key and openai-compatibility entries rather than from
// auth files, so no label table could resolve them. credentialFields then falls
// back to the raw per-credential ID, which is what makes a per-credential
// assertion possible at all — a resolved label would give both codex entries the
// same name and collapse the distinction.
//
// The anti-flap window is zero because one process-wide tracker outlives every
// test: a fifteen-minute window would make an entity's second degradation in the
// same run depend on the wall-clock spacing between the tests that drive it.
func newAlertTracker(sink *AlertSink) *alert.Tracker {
	return alert.New(sink, nil, 0, time.Now)
}
