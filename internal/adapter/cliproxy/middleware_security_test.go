package cliproxy

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

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
