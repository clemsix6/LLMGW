package cliproxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
	"github.com/gin-gonic/gin"
)

func TestMiddlewareDeniedBeforeDependencies(t *testing.T) {
	keys := &fakeKeys{}
	requests := &fakeRequests{}

	recorder := runMiddleware(t, http.MethodGet, "/management.html", nil, keys, requests, nil)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if keys.calls != 0 || requests.calls() != 0 {
		t.Fatalf("dependency calls = auth:%d repository:%d", keys.calls, requests.calls())
	}
}

func TestMiddlewarePublicPassesWithoutIdentity(t *testing.T) {
	keys := &fakeKeys{}
	requests := &fakeRequests{}
	var nextCalled bool

	recorder := runMiddleware(t, http.MethodGet, "/healthz", nil, keys, requests, func(c *gin.Context) {
		nextCalled = true
		if identity, ok := IdentityFromContext(c.Request.Context()); ok {
			t.Fatalf("public identity = %#v, want absent", identity)
		}
		c.Status(http.StatusNoContent)
	})

	if recorder.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("status = %d next = %v", recorder.Code, nextCalled)
	}
	if keys.calls != 0 || requests.calls() != 0 {
		t.Fatalf("dependency calls = auth:%d repository:%d", keys.calls, requests.calls())
	}
}

func TestMiddlewareCredentialFailuresAreIndistinguishable(t *testing.T) {
	unknown := validHeaders()
	cases := []struct {
		name    string
		headers http.Header
		keys    *fakeKeys
	}{
		{"missing", nil, &fakeKeys{}},
		{"malformed", http.Header{"Authorization": {"Basic raw-secret"}}, &fakeKeys{}},
		{"unknown", unknown, &fakeKeys{err: projectkey.ErrInvalidCredential}},
		{
			"duplicate",
			http.Header{"Authorization": {"Bearer raw-secret", "Bearer raw-secret"}},
			&fakeKeys{},
		},
		{
			"divergent",
			http.Header{
				"Authorization": {"Bearer raw-secret"},
				"X-Api-Key":     {"other-secret"},
			},
			&fakeKeys{},
		},
	}

	var wantBody string
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := runMiddleware(
				t,
				http.MethodPost,
				"/v1/messages",
				test.headers,
				test.keys,
				&fakeRequests{},
				nil,
			)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if strings.Contains(recorder.Body.String(), "raw-secret") ||
				strings.Contains(recorder.Body.String(), "other-secret") {
				t.Fatalf("401 body leaked credential: %s", recorder.Body.String())
			}
			if wantBody == "" {
				wantBody = recorder.Body.String()
			}
			if recorder.Body.String() != wantBody {
				t.Fatalf("body = %q, want indistinguishable %q", recorder.Body.String(), wantBody)
			}
		})
	}
}

func TestMiddlewareInfrastructureFailuresReturn503(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		keys     *fakeKeys
		requests *fakeRequests
	}{
		{
			name:     "authenticator",
			method:   http.MethodPost,
			path:     "/v1/messages",
			keys:     &fakeKeys{err: errors.New("database unavailable")},
			requests: &fakeRequests{},
		},
		{
			name:     "metadata repository",
			method:   http.MethodGet,
			path:     "/v1/models",
			keys:     validKeys(),
			requests: &fakeRequests{metadataErr: errors.New("database unavailable")},
		},
		{
			name:     "generation repository",
			method:   http.MethodPost,
			path:     "/v1/messages",
			keys:     validKeys(),
			requests: &fakeRequests{admitErr: errors.New("database unavailable")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var nextCalled bool
			recorder := runMiddleware(
				t,
				test.method,
				test.path,
				validHeaders(),
				test.keys,
				test.requests,
				func(c *gin.Context) { nextCalled = true },
			)
			if recorder.Code != http.StatusServiceUnavailable || nextCalled {
				t.Fatalf("status = %d next = %v, want 503 and no next", recorder.Code, nextCalled)
			}
			if strings.Contains(recorder.Body.String(), "database unavailable") {
				t.Fatalf("503 body leaked infrastructure error: %s", recorder.Body.String())
			}
		})
	}
}
