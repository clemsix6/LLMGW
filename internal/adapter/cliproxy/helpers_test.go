package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	"github.com/gin-gonic/gin"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// fixedTime pins the clock so admission and completion timestamps compare exactly.
var fixedTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// middlewareRequest is one drive through the governance middleware.
//
// It exists beside runMiddleware because the alert cases need a tracker and a
// caller-owned request context, which would otherwise widen every call site.
type middlewareRequest struct {
	method   string          // method is the HTTP method to drive.
	path     string          // path is the requested path.
	headers  http.Header     // headers carry the project credential.
	keys     *fakeKeys       // keys stands in for the project-key authenticator.
	requests *fakeRequests   // requests stands in for the governance repository.
	next     gin.HandlerFunc // next is the downstream handler, nil for a plain 200.
	tracker  *alert.Tracker  // tracker observes admissions, generations and database health.
	ctx      context.Context // ctx is the inbound request context, nil for the default.
	body     io.Reader       // body is the request body, nil for none.
	bridge   *UsageBridge    // bridge replaces the default one, to observe its capacity.
}

// runMiddlewareRequest drives one configured request through the middleware.
func runMiddlewareRequest(t *testing.T, spec middlewareRequest) *httptest.ResponseRecorder {
	t.Helper()
	bridge := spec.bridge
	if bridge == nil {
		bridge = fixedUsageBridge(t)
	}
	bridge.publishRecord = func(context.Context, sdkusage.Record) {}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(spec.method, spec.path, spec.body)
	request.Header = spec.headers.Clone()
	if spec.ctx != nil {
		request = request.WithContext(spec.ctx)
	}
	middleware := NewMiddleware(
		spec.keys,
		spec.requests,
		func() time.Time { return fixedTime },
		bridge,
		spec.tracker,
	)
	engine := gin.New()
	engine.Use(middleware.Handler())
	engine.Any("/*path", orStatusOK(spec.next))
	engine.ServeHTTP(recorder, request)
	return recorder
}

// orStatusOK defaults an unset downstream handler to a bare 200.
func orStatusOK(next gin.HandlerFunc) gin.HandlerFunc {
	if next != nil {
		return next
	}
	return func(c *gin.Context) {
		c.Status(http.StatusOK)
	}
}

// runMiddleware drives one request with no alert tracker.
func runMiddleware(
	t *testing.T,
	method string,
	path string,
	headers http.Header,
	keys *fakeKeys,
	requests *fakeRequests,
	next gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()
	return runMiddlewareRequest(t, middlewareRequest{
		method:   method,
		path:     path,
		headers:  headers,
		keys:     keys,
		requests: requests,
		next:     next,
	})
}

// validHeaders carries a well-formed project credential.
func validHeaders() http.Header {
	return http.Header{"Authorization": {"Bearer raw-secret"}}
}

// validKeys authenticates every credential as one known project.
func validKeys() *fakeKeys {
	return &fakeKeys{identity: governance.KeyIdentity{
		ProjectID:   42,
		ProjectName: "project-a",
		ClientKeyID: 7,
		KeyName:     "client-a",
		PublicID:    "public-1",
	}}
}

// fakeKeys stands in for the project-key authenticator.
type fakeKeys struct {
	identity    governance.KeyIdentity
	err         error
	calls       int
	credentials []string
}

// Authenticate records the credential and replays the configured outcome.
func (f *fakeKeys) Authenticate(
	_ context.Context,
	credential string,
) (governance.KeyIdentity, error) {
	f.calls++
	f.credentials = append(f.credentials, credential)
	return f.identity, f.err
}

// fakeRequests stands in for the governance request repository.
type fakeRequests struct {
	admission        governance.Admission
	admitErr         error
	metadataErr      error
	completeFailures int
	admitCalls       []governance.RequestEvent
	admitTimes       []time.Time
	metadataCalls    []governance.RequestEvent
	completeCalls    []completionCall
}

// AdmitGeneration records one admission attempt and replays the configured verdict.
func (f *fakeRequests) AdmitGeneration(
	_ context.Context,
	request governance.RequestEvent,
	now time.Time,
) (governance.Admission, error) {
	f.admitCalls = append(f.admitCalls, request)
	f.admitTimes = append(f.admitTimes, now)
	admission := f.admission
	if !admission.Allowed && len(admission.Blocks) == 0 {
		admission.Allowed = true
	}
	return admission, f.admitErr
}

// RecordMetadata records one unmetered request.
func (f *fakeRequests) RecordMetadata(
	_ context.Context,
	request governance.RequestEvent,
) error {
	f.metadataCalls = append(f.metadataCalls, request)
	return f.metadataErr
}

// CompleteRequest records one terminal status, failing the configured prefix.
func (f *fakeRequests) CompleteRequest(
	ctx context.Context,
	requestID string,
	status int,
	at time.Time,
) error {
	f.completeCalls = append(f.completeCalls, completionCall{
		requestID:  requestID,
		status:     status,
		at:         at,
		contextErr: ctx.Err(),
	})
	if len(f.completeCalls) <= f.completeFailures {
		return errors.New("completion unavailable")
	}
	return nil
}

// calls totals every repository interaction.
func (f *fakeRequests) calls() int {
	return len(f.admitCalls) + len(f.metadataCalls) + len(f.completeCalls)
}

// completionCall captures one CompleteRequest invocation.
type completionCall struct {
	requestID  string
	status     int
	at         time.Time
	contextErr error
}

// assertRunStillBlocked proves Run has not returned yet.
func assertRunStillBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Run returned before active work drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
}

// closedSignal reports readiness immediately.
func closedSignal() <-chan struct{} {
	signal := make(chan struct{})
	close(signal)
	return signal
}

// fakeLifecycle models the public SDK Run and Shutdown methods.
type fakeLifecycle struct {
	run      func(context.Context) error // run implements the fake SDK Run.
	shutdown func(context.Context) error // shutdown implements the fake SDK Shutdown.

	runEntered    chan struct{} // runEntered closes when the first Run begins.
	runEnterOnce  sync.Once     // runEnterOnce protects runEntered.
	runCalls      atomic.Int64  // runCalls counts SDK Run calls.
	shutdownCalls atomic.Int64  // shutdownCalls counts SDK Shutdown calls.
}

// Run records and delegates one fake SDK lifecycle.
func (f *fakeLifecycle) Run(ctx context.Context) error {
	f.runCalls.Add(1)
	f.runEnterOnce.Do(func() {
		close(f.runEntered)
	})
	return f.run(ctx)
}

// Shutdown records and delegates one fake SDK shutdown.
func (f *fakeLifecycle) Shutdown(ctx context.Context) error {
	f.shutdownCalls.Add(1)
	return f.shutdown(ctx)
}
