package cliproxy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

var fixedTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func TestMiddlewareReturnsStableBudgetBlock(t *testing.T) {
	resetAt := fixedTime.Add(20 * time.Minute)
	requests := &fakeRequests{admission: governance.Admission{
		Allowed: false,
		Blocks: []governance.BudgetBreach{{
			Limit: governance.BudgetLimit{
				Dimension: governance.DimensionCalls,
				Window:    governance.WindowHour,
			},
			ResetAt: resetAt,
		}},
	}}

	recorder := runMiddleware(
		t,
		http.MethodPost,
		"/v1/messages",
		validHeaders(),
		validKeys(),
		requests,
		nil,
	)

	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", recorder.Code)
	}
	want := `{"error":{"type":"budget_exceeded","dimension":"calls","window":"hour","reset_at":"2026-07-27T12:20:00Z"}}`
	if strings.TrimSpace(recorder.Body.String()) != want {
		t.Fatalf("body = %s, want %s", recorder.Body.String(), want)
	}
	if len(requests.completeCalls) != 0 {
		t.Fatalf("completion calls = %#v, want none", requests.completeCalls)
	}
}

func TestMiddlewareRecordsMetadataAndCompletesFinalStatus(t *testing.T) {
	requests := &fakeRequests{}
	var gotIdentity RequestIdentity

	recorder := runMiddleware(
		t,
		http.MethodGet,
		"/v1/models",
		validHeaders(),
		validKeys(),
		requests,
		func(c *gin.Context) {
			gotIdentity, _ = IdentityFromContext(c.Request.Context())
			c.Status(http.StatusNoContent)
		},
	)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	if len(requests.metadataCalls) != 1 || len(requests.admitCalls) != 0 {
		t.Fatalf("metadata = %#v admissions = %#v", requests.metadataCalls, requests.admitCalls)
	}
	assertRequest(t, requests.metadataCalls[0], governance.OperationMetadata, http.MethodGet, "/v1/models")
	assertIdentity(t, gotIdentity, requests.metadataCalls[0].ID, governance.OperationMetadata)
	assertCompletion(t, requests, requests.metadataCalls[0].ID, http.StatusNoContent)
}

func TestMiddlewareAdmitsGenerationAndLogsSafeWarning(t *testing.T) {
	requests := &fakeRequests{admission: governance.Admission{
		Allowed: true,
		Warnings: []governance.BudgetBreach{{
			Limit: governance.BudgetLimit{
				Dimension: governance.DimensionTokens,
				Window:    governance.WindowDay,
			},
			ResetAt: fixedTime.Add(time.Hour),
		}},
	}}
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalOutput) })

	var gotIdentity RequestIdentity
	recorder := runMiddleware(
		t,
		http.MethodPost,
		"/v1/messages",
		validHeaders(),
		validKeys(),
		requests,
		func(c *gin.Context) {
			gotIdentity, _ = IdentityFromContext(c.Request.Context())
			c.Status(http.StatusCreated)
		},
	)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", recorder.Code)
	}
	if len(requests.admitCalls) != 1 || len(requests.metadataCalls) != 0 {
		t.Fatalf("admissions = %#v metadata = %#v", requests.admitCalls, requests.metadataCalls)
	}
	assertRequest(t, requests.admitCalls[0], governance.OperationGeneration, http.MethodPost, "/v1/messages")
	assertIdentity(t, gotIdentity, requests.admitCalls[0].ID, governance.OperationGeneration)
	assertCompletion(t, requests, requests.admitCalls[0].ID, http.StatusCreated)
	if !strings.Contains(logs.String(), "project=42") {
		t.Fatalf("warning log = %q, want project ID", logs.String())
	}
	for _, secret := range []string{"raw-secret", "public-1"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("warning log leaked key material: %q", logs.String())
		}
	}
}

func TestMiddlewareCompletionRetriesWithoutRequestCancellation(t *testing.T) {
	requests := &fakeRequests{completeFailures: 2}
	ctx, cancel := context.WithCancel(context.Background())
	headers := validHeaders()
	var recorder *httptest.ResponseRecorder

	recorder = runMiddlewareContext(
		t,
		ctx,
		http.MethodPost,
		"/v1/messages",
		headers,
		validKeys(),
		requests,
		func(c *gin.Context) {
			c.Status(http.StatusAccepted)
			cancel()
		},
	)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	if len(requests.completeCalls) != 3 {
		t.Fatalf("completion attempts = %d, want 3", len(requests.completeCalls))
	}
	for _, call := range requests.completeCalls {
		if call.contextErr != nil {
			t.Fatalf("completion context error = %v", call.contextErr)
		}
	}
}

func TestMiddlewareGenerationCapacityRejectsBeforeAdmission(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	if !bridge.reserve("f5efc3a8-e6c3-49fd-bad6-6532fa51d216") {
		t.Fatal("failed to fill bridge capacity")
	}
	requests := &fakeRequests{}
	var nextCalled bool

	recorder := runMiddlewareWithBridge(
		t,
		context.Background(),
		http.MethodPost,
		"/v1/messages",
		validHeaders(),
		validKeys(),
		requests,
		func(*gin.Context) { nextCalled = true },
		bridge,
	)

	if recorder.Code != http.StatusServiceUnavailable || nextCalled {
		t.Fatalf("status/next = %d/%t, want 503/false", recorder.Code, nextCalled)
	}
	if len(requests.admitCalls) != 0 || len(requests.completeCalls) != 0 {
		t.Fatalf("full bridge repository calls = admit:%d complete:%d, want zero",
			len(requests.admitCalls), len(requests.completeCalls))
	}
	if strings.Contains(recorder.Body.String(), "budget_exceeded") {
		t.Fatalf("capacity response used budget semantics: %s", recorder.Body.String())
	}
}

func TestMiddlewareReleasesPreSDKFailuresAndPublishesAllowedBarrier(t *testing.T) {
	tests := []struct {
		name        string
		requests    *fakeRequests
		wantCode    int
		wantBarrier bool
	}{
		{
			name:     "admission failure",
			requests: &fakeRequests{admitErr: errors.New("database unavailable")},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "budget block",
			requests: &fakeRequests{admission: governance.Admission{
				Allowed: false,
				Blocks: []governance.BudgetBreach{{
					Limit: governance.BudgetLimit{
						Dimension: governance.DimensionCalls,
						Window:    governance.WindowHour,
					},
					ResetAt: fixedTime.Add(time.Hour),
				}},
			}},
			wantCode: http.StatusPaymentRequired,
		},
		{
			name:        "allowed",
			requests:    &fakeRequests{},
			wantCode:    http.StatusOK,
			wantBarrier: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := fixedUsageBridgeCapacity(t, 1)
			var barriers []sdkusage.Record
			bridge.publishRecord = func(_ context.Context, record sdkusage.Record) {
				barriers = append(barriers, record)
			}
			recorder := runMiddlewareWithBridge(
				t,
				context.Background(),
				http.MethodPost,
				"/v1/messages",
				validHeaders(),
				validKeys(),
				test.requests,
				nil,
				bridge,
			)

			if recorder.Code != test.wantCode {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantCode)
			}
			if test.wantBarrier {
				if bridge.outstanding() != 1 || len(barriers) != 1 {
					t.Fatalf("allowed outstanding/barriers = %d/%d, want 1/1",
						bridge.outstanding(), len(barriers))
				}
				requestID, ok := bridge.barrierRequestID(barriers[0].APIKey)
				if !ok || len(test.requests.admitCalls) != 1 ||
					requestID != test.requests.admitCalls[0].ID {
					t.Fatalf("published barrier request = (%q, %t)", requestID, ok)
				}
			} else if bridge.outstanding() != 0 || len(barriers) != 0 {
				t.Fatalf("pre-SDK failure outstanding/barriers = %d/%d, want zero",
					bridge.outstanding(), len(barriers))
			}
		})
	}
}

func TestMiddlewarePanicPublishesBarrierAndRepanics(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	var barrier sdkusage.Record
	bridge.publishRecord = func(_ context.Context, record sdkusage.Record) {
		barrier = record
	}
	requests := &fakeRequests{}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(NewMiddleware(validKeys(), requests, time.Now, bridge).Handler())
	engine.POST("/v1/messages", func(*gin.Context) {
		panic("handler panic")
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header = validHeaders()

	defer func() {
		value := recover()
		if value != "handler panic" {
			t.Fatalf("panic = %#v, want handler panic", value)
		}
		if bridge.outstanding() != 1 || barrier.APIKey == "" {
			t.Fatalf("panic outstanding/barrier = %d/%q, want 1/nonempty",
				bridge.outstanding(), barrier.APIKey)
		}
	}()
	engine.ServeHTTP(recorder, request)
}

func TestMiddlewareBarrierSamplesCancellationAfterDownstreamReturns(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	var controls []sdkusage.Record
	bridge.publishRecord = func(_ context.Context, record sdkusage.Record) {
		controls = append(controls, record)
	}
	ctx, cancel := context.WithCancel(context.Background())
	requests := &fakeRequests{}

	recorder := runMiddlewareWithBridge(
		t,
		ctx,
		http.MethodPost,
		"/v1/messages",
		validHeaders(),
		validKeys(),
		requests,
		func(c *gin.Context) {
			cancel()
			c.Status(http.StatusOK)
		},
		bridge,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if len(controls) != 2 {
		t.Fatalf("control records = %d, want cancel marker and barrier", len(controls))
	}
	cancelRequestID, cancelOK := bridge.cancelRequestID(controls[0].APIKey)
	requestID, canceled, ok := bridge.barrierState(controls[1].APIKey)
	if !cancelOK || cancelRequestID != requestID ||
		!ok || !canceled || len(requests.admitCalls) != 1 ||
		requestID != requests.admitCalls[0].ID {
		t.Fatalf("cancel/barrier state = (%q, %t)/(%q, %t, %t)",
			cancelRequestID, cancelOK, requestID, canceled, ok)
	}
}

func TestMiddlewareUnknownRouteAbortReleasesThroughBarrier(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {
			t.Fatal("unknown route persisted synthetic usage")
		}),
		bridge,
		nil,
	)
	bridge.publishRecord = plugin.HandleUsage
	requests := &fakeRequests{}

	recorder := runMiddlewareWithBridge(
		t,
		context.Background(),
		http.MethodGet,
		"/new-sdk-route",
		validHeaders(),
		validKeys(),
		requests,
		func(c *gin.Context) { c.AbortWithStatus(http.StatusNotFound) },
		bridge,
	)

	if recorder.Code != http.StatusNotFound || bridge.outstanding() != 0 {
		t.Fatalf("unknown route status/outstanding = %d/%d, want 404/0",
			recorder.Code, bridge.outstanding())
	}
	if len(requests.admitCalls) != 1 || len(requests.completeCalls) != 1 {
		t.Fatalf("unknown route lifecycle = admit:%d complete:%d, want 1/1",
			len(requests.admitCalls), len(requests.completeCalls))
	}
}

func runMiddleware(
	t *testing.T,
	method string,
	path string,
	headers http.Header,
	keys *fakeKeys,
	requests *fakeRequests,
	next gin.HandlerFunc,
) *httptest.ResponseRecorder {
	return runMiddlewareContext(t, context.Background(), method, path, headers, keys, requests, next)
}

func runMiddlewareContext(
	t *testing.T,
	ctx context.Context,
	method string,
	path string,
	headers http.Header,
	keys *fakeKeys,
	requests *fakeRequests,
	next gin.HandlerFunc,
) *httptest.ResponseRecorder {
	t.Helper()
	bridge := fixedUsageBridge(t)
	bridge.publishRecord = func(context.Context, sdkusage.Record) {}
	return runMiddlewareWithBridge(
		t, ctx, method, path, headers, keys, requests, next, bridge,
	)
}

func runMiddlewareWithBridge(
	t *testing.T,
	ctx context.Context,
	method string,
	path string,
	headers http.Header,
	keys *fakeKeys,
	requests *fakeRequests,
	next gin.HandlerFunc,
	bridge *UsageBridge,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, nil).WithContext(ctx)
	request.Header = headers.Clone()
	engine := gin.New()
	engine.Use(NewMiddleware(keys, requests, func() time.Time { return fixedTime }, bridge).Handler())
	if next == nil {
		next = func(c *gin.Context) {
			c.Status(http.StatusOK)
		}
	}
	engine.Any("/*path", next)
	engine.ServeHTTP(recorder, request)
	return recorder
}

func validHeaders() http.Header {
	return http.Header{"Authorization": {"Bearer raw-secret"}}
}

func validKeys() *fakeKeys {
	return &fakeKeys{identity: governance.KeyIdentity{
		ProjectID:   42,
		ProjectName: "project-a",
		ClientKeyID: 7,
		KeyName:     "client-a",
		PublicID:    "public-1",
	}}
}

func assertRequest(
	t *testing.T,
	request governance.RequestEvent,
	operation governance.Operation,
	method string,
	path string,
) {
	t.Helper()
	if request.ID == "" || request.ProjectID != 42 || request.ClientKeyID != 7 {
		t.Fatalf("request identity fields = %#v", request)
	}
	if request.Operation != operation || request.RequestedAt != fixedTime {
		t.Fatalf("request operation/time = %s/%s", request.Operation, request.RequestedAt)
	}
	if request.Method != method || request.Path != path {
		t.Fatalf("request route = %s %s", request.Method, request.Path)
	}
}

func assertIdentity(
	t *testing.T,
	identity RequestIdentity,
	requestID string,
	operation governance.Operation,
) {
	t.Helper()
	if identity.RequestID != requestID || identity.ProjectID != 42 || identity.ClientKeyID != 7 {
		t.Fatalf("request identity = %#v", identity)
	}
	if identity.KeyPublicID != "public-1" || identity.Operation != operation {
		t.Fatalf("request identity = %#v", identity)
	}
}

func assertCompletion(t *testing.T, requests *fakeRequests, requestID string, status int) {
	t.Helper()
	if len(requests.completeCalls) != 1 {
		t.Fatalf("completion calls = %#v", requests.completeCalls)
	}
	call := requests.completeCalls[0]
	if call.requestID != requestID || call.status != status || call.at != fixedTime {
		t.Fatalf("completion = %#v", call)
	}
}

type fakeKeys struct {
	identity    governance.KeyIdentity
	err         error
	calls       int
	credentials []string
}

func (f *fakeKeys) Authenticate(
	_ context.Context,
	credential string,
) (governance.KeyIdentity, error) {
	f.calls++
	f.credentials = append(f.credentials, credential)
	return f.identity, f.err
}

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

func (f *fakeRequests) RecordMetadata(
	_ context.Context,
	request governance.RequestEvent,
) error {
	f.metadataCalls = append(f.metadataCalls, request)
	return f.metadataErr
}

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

func (f *fakeRequests) calls() int {
	return len(f.admitCalls) + len(f.metadataCalls) + len(f.completeCalls)
}

type completionCall struct {
	requestID  string
	status     int
	at         time.Time
	contextErr error
}
