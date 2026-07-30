package alert_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// fixedNow is the instant every test clock starts from.
var fixedNow = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

// testClock is a manually advanced clock. Every case drives it instead of the
// wall clock, so the anti-flap window is deterministic and no test sleeps.
type testClock struct {
	mu  sync.Mutex // mu guards now against the concurrent observation case.
	now time.Time  // now is the instant the clock currently reports.
}

// newClock builds a clock frozen at fixedNow.
func newClock() *testClock {
	return &testClock{now: fixedNow}
}

// Now reports the current instant. It is what a tracker is built with.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// Advance moves the clock forward by delta.
func (c *testClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(delta)
}

// recordingNotifier collects every event a tracker offers it.
//
// The tracker holds its mutex across Notify, so this notifier neither blocks
// nor calls back into the tracker.
type recordingNotifier struct {
	mu       sync.Mutex    // mu guards the fields against concurrent observation.
	accepted bool          // accepted is what Notify reports to the tracker.
	events   []alert.Event // events are every event offered, in delivery order.
}

// newNotifier builds a notifier accepting everything offered to it.
func newNotifier() *recordingNotifier {
	return &recordingNotifier{accepted: true}
}

// Notify records the event and reports the programmed acceptance.
func (n *recordingNotifier) Notify(event alert.Event) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.events = append(n.events, event)
	return n.accepted
}

// setAccepted programs what later Notify calls report to the tracker.
func (n *recordingNotifier) setAccepted(accepted bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.accepted = accepted
}

// events returns a copy of what was offered, in delivery order.
func (n *recordingNotifier) recorded() []alert.Event {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]alert.Event(nil), n.events...)
}

// kinds returns the offered kinds, in delivery order.
func (n *recordingNotifier) kinds() []alert.Kind {
	recorded := n.recorded()

	kinds := make([]alert.Kind, len(recorded))
	for index, event := range recorded {
		kinds[index] = event.Kind
	}
	return kinds
}

// newTracker builds a tracker over sink with the production anti-flap window.
func newTracker(sink alert.Notifier, clock *testClock) *alert.Tracker {
	return alert.New(sink, nil, alert.DefaultWindow, clock.Now)
}

// assertKinds fails unless exactly these kinds were offered, in this order.
func assertKinds(t *testing.T, sink *recordingNotifier, want ...alert.Kind) {
	t.Helper()

	got := sink.kinds()
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

// eventAt returns the recorded event at index, failing when it is missing.
func eventAt(t *testing.T, sink *recordingNotifier, index int) alert.Event {
	t.Helper()

	recorded := sink.recorded()
	if index >= len(recorded) {
		t.Fatalf("event %d missing, recorded %d", index, len(recorded))
	}
	return recorded[index]
}

// assertFieldNames fails unless the event carries exactly these labels, in this
// order. It is what pins the privacy contract: a field carrying something it
// should not can only reach Discord by appearing here first.
func assertFieldNames(t *testing.T, event alert.Event, want ...string) {
	t.Helper()

	got := make([]string, len(event.Fields))
	for index, field := range event.Fields {
		got[index] = field.Name
	}

	if len(got) != len(want) {
		t.Fatalf("%s fields = %v, want %v", event.Kind, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s fields = %v, want %v", event.Kind, got, want)
		}
	}
}

// fieldValue returns the value of the named field, or "" when it is absent.
func fieldValue(event alert.Event, name string) string {
	for _, field := range event.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

func TestTrackerEmitsOncePerTransition(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.ObserveAttempt("claude", "cred-1", "claude-opus-5", true, 429)
	tracker.ObserveAttempt("claude", "cred-1", "claude-opus-5", true, 429)

	assertKinds(t, sink, alert.KindCredentialRateLimited)

	event := eventAt(t, sink, 0)
	if event.Severity != alert.SeverityWarning {
		t.Fatalf("severity = %q, want %q", event.Severity, alert.SeverityWarning)
	}
	if !event.At.Equal(fixedNow) {
		t.Fatalf("at = %s, want %s", event.At, fixedNow)
	}
}

func TestEmitCarriesOnlyTheCallerFields(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	tracker.Emit(alert.KindGatewayStarted)
	tracker.Emit(alert.KindProjectKeyCreated, alert.Field{Name: "Project", Value: "alpha"})

	assertKinds(t, sink, alert.KindGatewayStarted, alert.KindProjectKeyCreated)
	assertFieldNames(t, eventAt(t, sink, 0))
	assertFieldNames(t, eventAt(t, sink, 1), "Project")

	if eventAt(t, sink, 0).Severity != alert.SeverityInfo {
		t.Fatalf("severity = %q, want %q", eventAt(t, sink, 0).Severity, alert.SeverityInfo)
	}
}

// TestWindowSuppressesAFlapAndReEmitsAfterIt drives the flap the escalation rule
// deliberately refuses to let through: the second rate limit follows a delivered
// recovery, so the entity is healthy and only the window can release it.
func TestWindowSuppressesAFlapAndReEmitsAfterIt(t *testing.T) {
	sink := newNotifier()
	clock := newClock()
	tracker := newTracker(sink, clock)

	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)
	clock.Advance(time.Minute)
	tracker.ObserveAttempt("claude", "cred-1", "opus", false, 0)

	assertKinds(t, sink, alert.KindCredentialRateLimited, alert.KindCredentialRecovered)

	clock.Advance(time.Minute)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)

	assertKinds(t, sink, alert.KindCredentialRateLimited, alert.KindCredentialRecovered)

	clock.Advance(alert.DefaultWindow)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)

	assertKinds(t,
		sink,
		alert.KindCredentialRateLimited,
		alert.KindCredentialRecovered,
		alert.KindCredentialRateLimited,
	)
}

// TestRejectedNotifyLeavesDeliveredBehind pins that a dropped event is deferred
// rather than lost: the very next identical observation offers it again.
func TestRejectedNotifyLeavesDeliveredBehind(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	sink.setAccepted(false)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 401)

	sink.setAccepted(true)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 401)
	tracker.ObserveAttempt("claude", "cred-1", "opus", true, 401)

	assertKinds(t, sink, alert.KindCredentialUnauthorized, alert.KindCredentialUnauthorized)
}

// TestDisabledTrackerAcceptsEveryObservation covers the two shapes alerting is
// disabled in: no tracker at all, and a tracker with no notifier.
func TestDisabledTrackerAcceptsEveryObservation(t *testing.T) {
	disabled := map[string]*alert.Tracker{
		"nil tracker":  nil,
		"nil notifier": alert.New(nil, nil, alert.DefaultWindow, newClock().Now),
	}

	for name, tracker := range disabled {
		t.Run(name, func(t *testing.T) {
			breach := budgetBreach(governance.DimensionCalls, governance.WindowHour, governance.ActionBlock, 100)

			tracker.ObserveAttempt("claude", "cred-1", "opus", true, 429)
			tracker.ObserveGeneration(500)
			tracker.ObserveAdmission("alpha", []governance.BudgetBreach{breach}, nil)
			tracker.ObserveProjectKeys([]governance.KeyInfo{expiringKey("pk-1")}, fixedNow)
			tracker.ObserveDatabase(false)
			tracker.ObserveDatabase(true)
			tracker.Emit(alert.KindGatewayStarted)
		})
	}
}

// TestConcurrentObservationsAreRaceFree proves the tracker's mutex holds when
// the observation points it is wired to run in parallel.
func TestConcurrentObservationsAreRaceFree(t *testing.T) {
	sink := newNotifier()
	tracker := newTracker(sink, newClock())

	var running sync.WaitGroup
	running.Add(50)

	for worker := range 50 {
		go func(worker int) {
			defer running.Done()

			credential := "cred-" + strconv.Itoa(worker%5)
			for range 20 {
				tracker.ObserveAttempt("claude", credential, "opus", true, 429)
				tracker.ObserveGeneration(503)
				tracker.ObserveAttempt("claude", credential, "opus", false, 0)
				tracker.ObserveGeneration(200)
			}
		}(worker)
	}
	running.Wait()

	if len(sink.kinds()) == 0 {
		t.Fatal("no event delivered under concurrent observation")
	}
}
