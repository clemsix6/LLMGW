package cliproxy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	"github.com/google/uuid"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// recordingNotifier collects every event a tracker delivers.
type recordingNotifier struct {
	mu     sync.Mutex    // mu guards events against concurrent deliveries.
	events []alert.Event // events are the accepted events, in delivery order.
}

// Notify accepts and records one event.
func (r *recordingNotifier) Notify(event alert.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
	return true
}

// total returns how many events were recorded.
func (r *recordingNotifier) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.events)
}

// countOf returns how many recorded events carry the kind.
func (r *recordingNotifier) countOf(kind alert.Kind) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := 0
	for _, event := range r.events {
		if event.Kind == kind {
			total++
		}
	}
	return total
}

// first returns the first recorded event of the kind.
func (r *recordingNotifier) first(kind alert.Kind) (alert.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, event := range r.events {
		if event.Kind == kind {
			return event, true
		}
	}
	return alert.Event{}, false
}

// observingTracker builds a tracker with no anti-flap window, so consecutive
// transitions of one entity stay observable inside a single test.
func observingTracker() (*alert.Tracker, *recordingNotifier) {
	sink := &recordingNotifier{}
	return alert.New(sink, nil, 0, time.Now), sink
}

// fieldValue returns the value of the named field, or the empty string.
func fieldValue(event alert.Event, name string) string {
	for _, field := range event.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

// TestAlertUsagePluginObservesUpstreamStatus proves one SDK usage record
// becomes one credential observation carrying the upstream identity.
func TestAlertUsagePluginObservesUpstreamStatus(t *testing.T) {
	tracker, sink := observingTracker()

	NewAlertUsagePlugin(tracker).HandleUsage(context.Background(), sdkusage.Record{
		Provider: "openai-compatibility",
		Model:    "upstream-model",
		AuthID:   "account-a",
		Failed:   true,
		Fail:     sdkusage.Failure{StatusCode: http.StatusTooManyRequests},
	})

	event, found := sink.first(alert.KindCredentialRateLimited)
	if !found {
		t.Fatalf("events = %d, want one credential_rate_limited", sink.total())
	}
	if fieldValue(event, "Provider") != "openai-compatibility" ||
		fieldValue(event, "Credential") != "account-a" ||
		fieldValue(event, "Model") != "upstream-model" ||
		fieldValue(event, "Status") != "429" {
		t.Fatalf("observed fields = %#v", event.Fields)
	}
}

// TestAlertUsagePluginToleratesEmptyRecordAndNilTracker proves the plugin never
// panics on a zero-valued record and stays usable when alerting is disabled.
func TestAlertUsagePluginToleratesEmptyRecordAndNilTracker(t *testing.T) {
	tracker, sink := observingTracker()

	NewAlertUsagePlugin(tracker).HandleUsage(context.Background(), sdkusage.Record{})
	NewAlertUsagePlugin(nil).HandleUsage(context.Background(), sdkusage.Record{
		Provider: "claude",
		AuthID:   "account-a",
		Failed:   true,
		Fail:     sdkusage.Failure{StatusCode: http.StatusUnauthorized},
	})

	if sink.total() != 0 {
		t.Fatalf("events = %d, want none", sink.total())
	}
}

// TestControlRecordsNeverReachCredentialObservation proves the LLMGW barrier
// and cancellation markers are filtered before the alert plugin sees them.
// Without it a control record would be reported as a provider attempt.
func TestControlRecordsNeverReachCredentialObservation(t *testing.T) {
	tracker, sink := observingTracker()
	bridge := fixedUsageBridge(t)
	requestID := uuid.NewString()
	if !bridge.reserve(requestID) {
		t.Fatal("reserve failed")
	}

	barrier, ok := bridge.barrierFor(requestID, false)
	if !ok {
		t.Fatal("barrier token")
	}
	cancel, ok := bridge.cancel(requestID)
	if !ok {
		t.Fatal("cancel token")
	}

	plugin := nonBarrierUsagePlugin{bridge: bridge, next: NewAlertUsagePlugin(tracker)}
	for _, token := range []string{barrier, cancel} {
		plugin.HandleUsage(context.Background(), sdkusage.Record{
			APIKey:   token,
			Provider: "claude",
			Model:    "claude-opus-5",
			AuthID:   "account-a",
			Failed:   true,
			Fail:     sdkusage.Failure{StatusCode: http.StatusTooManyRequests},
		})
	}

	if sink.total() != 0 {
		t.Fatalf("events = %d, want none from control records", sink.total())
	}
}

// TestServiceCompositionAcceptsTheAlertPlugin proves the alert plugin passes
// through the extra-plugin slot while a second durable usage plugin is still
// rejected — the reason the observer is its own type.
func TestServiceCompositionAcceptsTheAlertPlugin(t *testing.T) {
	bridge := fixedUsageBridge(t)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge, nil)
	usagePlugin := NewUsagePlugin(nil, bridge, nil)
	cfg := config.Config{LLMGW: config.LLMGW{UsageOutstandingCapacity: bridge.capacity}}

	alertPlugin := NewAlertUsagePlugin(nil)
	if err := validateServiceComposition(
		cfg, middleware, usagePlugin, []sdkusage.Plugin{alertPlugin},
	); err != nil {
		t.Fatalf("alert plugin rejected: %v", err)
	}
	if err := validateServiceComposition(
		cfg, middleware, usagePlugin, []sdkusage.Plugin{usagePlugin},
	); err == nil {
		t.Fatal("a second LLMGW usage plugin was accepted")
	}
}
