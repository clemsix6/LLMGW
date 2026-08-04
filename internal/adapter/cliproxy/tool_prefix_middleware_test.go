package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/gin-gonic/gin"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// toolDeclaration is one request body declaring a single tool.
const toolDeclaration = `{"tools":[{"name":"search_web"}]}`

// TestToolPrefixEngagesOnlyForTheRightRouteAndFlag proves the rewrite reaches
// exactly the two Anthropic routes of a flagged project, and that a project
// without the flag keeps today's path: its body is never read and its writer
// is never wrapped.
func TestToolPrefixEngagesOnlyForTheRightRouteAndFlag(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		enabled  bool
		wantBody string
		wantWrap bool
	}{
		{
			name:     "messages with the flag",
			method:   http.MethodPost,
			path:     "/v1/messages",
			enabled:  true,
			wantBody: `{"tools":[{"name":"new_search_web"}]}`,
			wantWrap: true,
		},
		{
			name:     "count_tokens with the flag",
			method:   http.MethodPost,
			path:     "/v1/messages/count_tokens",
			enabled:  true,
			wantBody: `{"tools":[{"name":"new_search_web"}]}`,
			wantWrap: false,
		},
		{
			name:     "messages without the flag",
			method:   http.MethodPost,
			path:     "/v1/messages",
			enabled:  false,
			wantBody: toolDeclaration,
			wantWrap: false,
		},
		{
			name:     "models with the flag",
			method:   http.MethodGet,
			path:     "/v1/models",
			enabled:  true,
			wantBody: toolDeclaration,
			wantWrap: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := validKeys()
			keys.identity.PrefixToolNames = test.enabled

			var seenBody string
			var wrapped bool
			runMiddlewareRequest(t, middlewareRequest{
				method:   test.method,
				path:     test.path,
				headers:  validHeaders(),
				keys:     keys,
				requests: &fakeRequests{},
				body:     strings.NewReader(toolDeclaration),
				next: func(c *gin.Context) {
					payload, _ := io.ReadAll(c.Request.Body)
					seenBody = string(payload)
					_, wrapped = c.Writer.(*toolPrefixWriter)
					c.Status(http.StatusOK)
				},
			})

			if seenBody != test.wantBody {
				t.Fatalf("handler read %s, want %s", seenBody, test.wantBody)
			}
			if wrapped != test.wantWrap {
				t.Fatalf("response writer wrapped = %v, want %v", wrapped, test.wantWrap)
			}
		})
	}
}

// TestFlaggedResponseIsWrittenBeforeCompletionIsRecorded proves both halves of
// the wiring at once: the client receives the response with the prefix
// stripped, and the wrapper's finalization runs before completion, so the
// request is never recorded complete against a body still held in memory.
func TestFlaggedResponseIsWrittenBeforeCompletionIsRecorded(t *testing.T) {
	recorder := httptest.NewRecorder()
	requests := &completionOrder{fakeRequests: &fakeRequests{}, recorder: recorder}

	runFlaggedGeneration(t, requests, recorder, func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		_, _ = c.Writer.Write([]byte(toolUseResponse))
	})

	body := recorder.Body.String()
	if !strings.Contains(body, `"name":"search_web"`) || strings.Contains(body, "new_search_web") {
		t.Fatalf("client received %s, want the prefix stripped", body)
	}
	if requests.bodyAtCompletion != body {
		t.Fatalf("completion saw %q on the wire, want the finished body %q",
			requests.bodyAtCompletion, body)
	}
}

// TestOversizedFlaggedRequestReturnsThePermit proves the 32 MiB refusal gives
// the generation permit back. The bridge's capacity is finite and a leaked
// permit is never recovered, so enough refusals would poison it and stop the
// service.
func TestOversizedFlaggedRequestReturnsThePermit(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	keys := validKeys()
	keys.identity.PrefixToolNames = true

	var nextCalled bool
	refused := runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     keys,
		requests: &fakeRequests{},
		bridge:   bridge,
		body:     io.LimitReader(endlessReader{}, maxRewriteBody+1),
		next:     func(*gin.Context) { nextCalled = true },
	})

	if refused.Code != http.StatusRequestEntityTooLarge || nextCalled {
		t.Fatalf("status = %d next = %v, want 413 and no next", refused.Code, nextCalled)
	}
	if !strings.Contains(refused.Body.String(), "request_too_large") {
		t.Fatalf("body = %s, want the request_too_large type", refused.Body.String())
	}
	if bridge.outstanding() != 0 {
		t.Fatalf("bridge holds %d permits after a refusal, want 0", bridge.outstanding())
	}

	// The capacity is genuinely back: the same one-permit bridge admits again.
	admitted := runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodPost,
		path:     "/v1/messages",
		headers:  validHeaders(),
		keys:     keys,
		requests: &fakeRequests{},
		bridge:   bridge,
		body:     strings.NewReader(toolDeclaration),
	})
	if admitted.Code != http.StatusOK {
		t.Fatalf("status = %d after the refusal, want 200 from a restored bridge", admitted.Code)
	}
}

// TestDeclaredOversizedBodyIsRefusedUnread proves a body whose declared length
// is already past the ceiling is refused without being buffered at all.
func TestDeclaredOversizedBodyIsRefusedUnread(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(toolDeclaration))
	c.Request.ContentLength = maxRewriteBody + 1

	if rewriteRequestBody(c, requestRewrite{prefixToolNames: true}) {
		t.Fatal("rewriteRequestBody = true for a declared oversized body, want a refusal")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
	unread, _ := io.ReadAll(c.Request.Body)
	if string(unread) != toolDeclaration {
		t.Fatalf("body was consumed before the refusal: read %q", unread)
	}
}

// runFlaggedGeneration drives one flagged /v1/messages request through the
// middleware against a caller-owned recorder and repository.
func runFlaggedGeneration(
	t *testing.T,
	requests governance.RequestRepository,
	recorder *httptest.ResponseRecorder,
	next gin.HandlerFunc,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	bridge := fixedUsageBridge(t)
	bridge.publishRecord = func(context.Context, sdkusage.Record) {}
	keys := validKeys()
	keys.identity.PrefixToolNames = true

	middleware := NewMiddleware(keys, requests, func() time.Time { return fixedTime }, bridge, nil)
	engine := gin.New()
	engine.Use(middleware.Handler())
	engine.POST("/v1/messages", next)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(toolDeclaration))
	request.Header = validHeaders()
	engine.ServeHTTP(recorder, request)
}

// completionOrder records what the client had already received at the moment
// the request was recorded complete.
type completionOrder struct {
	*fakeRequests                               // fakeRequests supplies admission and metadata.
	recorder         *httptest.ResponseRecorder // recorder holds what reached the client.
	bodyAtCompletion string                     // bodyAtCompletion is the wire content at completion.
}

// CompleteRequest captures the wire content before delegating.
func (o *completionOrder) CompleteRequest(
	ctx context.Context,
	requestID string,
	status int,
	at time.Time,
) error {
	o.bodyAtCompletion = o.recorder.Body.String()
	return o.fakeRequests.CompleteRequest(ctx, requestID, status, at)
}

// endlessReader supplies a body that never ends, without allocating one.
type endlessReader struct{}

// Read fills the caller's buffer and never fails.
func (endlessReader) Read(p []byte) (int, error) {
	return len(p), nil
}
