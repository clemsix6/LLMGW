package alert_test

import (
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// fixedNow is the frozen observation clock the smoke test runs against.
var fixedNow = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

// recordingNotifier collects the events a tracker delivers.
type recordingNotifier struct {
	mu     sync.Mutex    // mu guards events against concurrent observation.
	events []alert.Event // events are the accepted events, in delivery order.
}

// Notify records the event and reports it as accepted.
func (n *recordingNotifier) Notify(event alert.Event) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.events = append(n.events, event)
	return true
}

func TestTrackerEmitsOncePerTransition(t *testing.T) {
	sink := &recordingNotifier{}
	tracker := alert.New(sink, nil, alert.DefaultWindow, func() time.Time { return fixedNow })

	tracker.ObserveAttempt("claude", "cred-1", "claude-opus-5", true, 429)
	tracker.ObserveAttempt("claude", "cred-1", "claude-opus-5", true, 429)

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	if sink.events[0].Kind != alert.KindCredentialRateLimited {
		t.Fatalf("kind = %q, want %q", sink.events[0].Kind, alert.KindCredentialRateLimited)
	}
	if sink.events[0].Severity != alert.SeverityWarning {
		t.Fatalf("severity = %q, want %q", sink.events[0].Severity, alert.SeverityWarning)
	}
}
