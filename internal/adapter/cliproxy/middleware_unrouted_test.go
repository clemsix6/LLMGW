package cliproxy

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestUnmatchedPathIsRejectedWithoutAccounting pins the two halves of the
// route-existence policy against a router that answers exactly like the SDK's:
// a registered path LLMGW never classified is still admitted and metered, so a
// generation surface a later SDK version adds cannot slip through unaccounted,
// while a path the router matches nowhere is refused before anything is
// recorded. Recording it is what used to leave a request no provider ever saw
// looking like a generation whose usage was lost.
func TestUnmatchedPathIsRejectedWithoutAccounting(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantAdmits int
	}{
		{
			name:       "registered but unclassified path is metered",
			path:       "/sdk-added-route",
			wantStatus: http.StatusOK,
			wantAdmits: 1,
		},
		{
			name:       "unmatched path is refused unrecorded",
			path:       "/api/tags",
			wantStatus: http.StatusNotFound,
			wantAdmits: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := &fakeRequests{}
			recorder := runMiddlewareRequest(t, middlewareRequest{
				method:   http.MethodGet,
				path:     test.path,
				headers:  validHeaders(),
				keys:     validKeys(),
				requests: requests,
				register: func(engine *gin.Engine) {
					engine.GET("/sdk-added-route", func(c *gin.Context) {
						c.Status(http.StatusOK)
					})
				},
			})
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			if len(requests.admitCalls) != test.wantAdmits {
				t.Fatalf("admissions = %d, want %d", len(requests.admitCalls), test.wantAdmits)
			}
			if test.wantAdmits == 0 && requests.calls() != 0 {
				t.Fatalf("repository calls = %d, want none", requests.calls())
			}
		})
	}
}

// TestUnmatchedPathStillAuthenticatesFirst proves an unknown path answers an
// anonymous caller exactly like a known one. Refusing it before authentication
// would turn the gateway's 404 into a route oracle for anyone with no key.
func TestUnmatchedPathStillAuthenticatesFirst(t *testing.T) {
	requests := &fakeRequests{}
	recorder := runMiddlewareRequest(t, middlewareRequest{
		method:   http.MethodGet,
		path:     "/api/tags",
		headers:  http.Header{},
		keys:     validKeys(),
		requests: requests,
		register: func(engine *gin.Engine) {
			engine.GET("/v1/models", func(c *gin.Context) { c.Status(http.StatusOK) })
		},
	})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if requests.calls() != 0 {
		t.Fatalf("repository calls = %d, want none", requests.calls())
	}
}
