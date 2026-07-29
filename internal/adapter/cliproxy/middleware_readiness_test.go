package cliproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestRequestsAreRefusedUntilTheSDKIsReady proves the gateway never serves
// traffic through a listener the SDK has opened before finishing startup.
// Without this, every restart answers 502 to real generations while the health
// route already advertises a healthy gateway.
func TestRequestsAreRefusedUntilTheSDKIsReady(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ready := make(chan struct{})
	bridge := fixedUsageBridgeCapacity(t, 1)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge)
	middleware.serveWhenReady(ready)

	engine := gin.New()
	engine.Use(middleware.Handler())
	engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/v1/messages"},
	} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s before readiness = %d, want %d",
				route.method, route.path, recorder.Code, http.StatusServiceUnavailable)
		}
	}

	close(ready)

	healthy := httptest.NewRecorder()
	engine.ServeHTTP(healthy, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthy.Code != http.StatusOK {
		t.Fatalf("/healthz after readiness = %d, want %d", healthy.Code, http.StatusOK)
	}
}
