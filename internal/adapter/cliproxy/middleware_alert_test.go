package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	"github.com/gin-gonic/gin"
)

// TestMiddlewareWithoutATrackerServesNormally proves alerting stays optional:
// a nil tracker needs no guard at any observation point.
func TestMiddlewareWithoutATrackerServesNormally(t *testing.T) {
	requests := &fakeRequests{}

	recorder := runMiddleware(
		t, http.MethodPost, "/v1/messages", validHeaders(), validKeys(), requests, nil,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if len(requests.completeCalls) != 1 {
		t.Fatalf("completion calls = %d, want 1", len(requests.completeCalls))
	}
}

// TestBlockedGenerationAdmissionIsObservedBeforeTheAbort proves a denied
// admission still reports its blocks. Observing after the budget abort would
// leave every observed admission allowed, so budget_blocked could never fire.
func TestBlockedGenerationAdmissionIsObservedBeforeTheAbort(t *testing.T) {
	tracker, sink := observingTracker()

	recorder := runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: blockingRepository(),
		tracker:  tracker,
	})

	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusPaymentRequired)
	}
	event, found := sink.first(alert.KindBudgetBlocked)
	if !found {
		t.Fatalf("events = %d, want one budget_blocked", sink.total())
	}
	if fieldValue(event, "Project") != "project-a" ||
		fieldValue(event, "Dimension") != string(governance.DimensionCalls) ||
		fieldValue(event, "Window") != string(governance.WindowDay) {
		t.Fatalf("observed fields = %#v", event.Fields)
	}
}

// TestMetadataRequestsAreNeverObservedAsAdmissions proves the synthetic
// always-allowed metadata admission never reaches the tracker: observing one
// would clear a budget while generations are still being denied.
func TestMetadataRequestsAreNeverObservedAsAdmissions(t *testing.T) {
	tracker, sink := observingTracker()

	runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: blockingRepository(),
		tracker:  tracker,
	})
	if sink.countOf(alert.KindBudgetBlocked) != 1 {
		t.Fatalf("budget_blocked events = %d, want 1", sink.countOf(alert.KindBudgetBlocked))
	}

	runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodGet,
		path:     "/v1/models",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: &fakeRequests{},
		tracker:  tracker,
	})

	if sink.total() != 1 {
		t.Fatalf("a metadata request produced %d further events, want none", sink.total()-1)
	}
}

// TestRepositoryFailureReportsTheDatabaseOnlyForALiveCaller proves a client
// that walked away cannot page the operator: its cancellation aborts the query
// and says nothing about PostgreSQL.
func TestRepositoryFailureReportsTheDatabaseOnlyForALiveCaller(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cancelledTracker, cancelledSink := observingTracker()
	runMiddlewareRequest(t, failingAdmission(cancelledTracker, cancelled))
	if cancelledSink.countOf(alert.KindDatabaseUnavailable) != 0 {
		t.Fatalf("a cancelled caller's repository error was reported as an outage")
	}

	liveTracker, liveSink := observingTracker()
	runMiddlewareRequest(t, failingAdmission(liveTracker, nil))
	if liveSink.countOf(alert.KindDatabaseUnavailable) != 1 {
		t.Fatalf(
			"database_unavailable events = %d, want 1",
			liveSink.countOf(alert.KindDatabaseUnavailable),
		)
	}
}

// TestMetadataRecordingReportsDatabaseHealth proves the metadata branch of
// request recording reports both outcomes, which is what makes a restore
// unmissable on a gateway serving metadata between generations.
func TestMetadataRecordingReportsDatabaseHealth(t *testing.T) {
	tracker, sink := observingTracker()

	runMiddlewareRequest(t, metadataRequest(tracker, &fakeRequests{
		metadataErr: errors.New("database unavailable"),
	}))
	if sink.countOf(alert.KindDatabaseUnavailable) != 1 {
		t.Fatalf("a metadata repository failure did not report the database down")
	}

	// Completion fails every attempt here, so a restore can only come from the
	// metadata recording that preceded it.
	runMiddlewareRequest(t, metadataRequest(tracker, &fakeRequests{
		completeFailures: completionAttempts,
	}))
	if sink.countOf(alert.KindDatabaseRestored) != 1 {
		t.Fatalf("metadata recording did not report the database healthy")
	}
	if sink.countOf(alert.KindDatabaseUnavailable) != 2 {
		t.Fatalf("an exhausted completion did not report the database down")
	}
}

// TestRetriedCompletionDoesNotReportTheDatabaseDown proves a completion that
// fails once and succeeds on retry stays silent. Reporting per attempt would
// page the operator with database_unavailable followed by database_restored
// for a blip the retry loop exists to absorb.
func TestRetriedCompletionDoesNotReportTheDatabaseDown(t *testing.T) {
	tracker, sink := observingTracker()

	runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: &fakeRequests{completeFailures: 1},
		tracker:  tracker,
	})

	if sink.total() != 0 {
		t.Fatalf("events = %d, want none from a completion that succeeded on retry", sink.total())
	}
}

// TestConsecutiveGenerationFailuresAreObservedFromTheFinalStatus proves the
// observed status is the one the downstream handler wrote, not the gin default
// the writer carries before c.Next runs: the third 5xx reports the outage, and
// a served generation resets the count.
func TestConsecutiveGenerationFailuresAreObservedFromTheFinalStatus(t *testing.T) {
	tracker, sink := observingTracker()
	failing := func(c *gin.Context) { c.Status(http.StatusInternalServerError) }

	for attempt := 1; attempt <= 2; attempt++ {
		driveGeneration(t, tracker, failing)
		if sink.countOf(alert.KindGenerationFailures) != 0 {
			t.Fatalf("generation_failures reported after %d consecutive failures", attempt)
		}
	}

	driveGeneration(t, tracker, failing)
	event, found := sink.first(alert.KindGenerationFailures)
	if !found {
		t.Fatalf("the third failing generation reported nothing")
	}
	if fieldValue(event, "Last status") != "500" {
		t.Fatalf("observed status = %q, want the downstream 500", fieldValue(event, "Last status"))
	}

	driveGeneration(t, tracker, nil)
	if sink.countOf(alert.KindGenerationRecovered) != 1 {
		t.Fatalf("a served generation reported no recovery")
	}

	driveGeneration(t, tracker, failing)
	driveGeneration(t, tracker, failing)
	if sink.countOf(alert.KindGenerationFailures) != 1 {
		t.Fatalf("a served generation did not reset the consecutive failure count")
	}
}

// driveGeneration runs one admitted generation ending in the next handler.
func driveGeneration(
	t *testing.T,
	tracker *alert.Tracker,
	next gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()
	return runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: &fakeRequests{},
		next:     next,
		tracker:  tracker,
	})
}

// metadataRequest describes one metadata drive against the given repository.
func metadataRequest(tracker *alert.Tracker, requests *fakeRequests) middlewareRequest {
	return middlewareRequest{
		method:   http.MethodGet,
		path:     "/v1/models",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: requests,
		tracker:  tracker,
	}
}

// failingAdmission describes one generation whose admission cannot be recorded.
func failingAdmission(tracker *alert.Tracker, ctx context.Context) middlewareRequest {
	return middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     validKeys(),
		requests: &fakeRequests{admitErr: errors.New("database unavailable")},
		tracker:  tracker,
		ctx:      ctx,
	}
}

// blockingRepository denies admission with one exhausted daily calls budget.
func blockingRepository() *fakeRequests {
	return &fakeRequests{admission: governance.Admission{
		Blocks: []governance.BudgetBreach{{
			Limit: governance.BudgetLimit{
				Dimension: governance.DimensionCalls,
				Window:    governance.WindowDay,
				Action:    governance.ActionBlock,
				MaxValue:  10,
			},
			ResetAt: fixedTime.Add(time.Hour),
		}},
	}}
}
