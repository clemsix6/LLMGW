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

// TestForbiddenRoutesStayForbiddenBeforeReadiness proves the deny policy is
// unconditional and never weakened by a transient startup state.
func TestForbiddenRoutesStayForbiddenBeforeReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, fixedUsageBridgeCapacity(t, 1))
	middleware.serveWhenReady(make(chan struct{}))

	engine := gin.New()
	engine.Use(middleware.Handler())

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v0/management/config", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("denied route before readiness = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

// TestNoReadinessSignalServesImmediately keeps a middleware built without a
// readiness signal usable, which is what unit tests and any simpler
// composition rely on.
func TestNoReadinessSignalServesImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, fixedUsageBridgeCapacity(t, 1))

	engine := gin.New()
	engine.Use(middleware.Handler())
	engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/healthz without a readiness signal = %d, want %d", recorder.Code, http.StatusOK)
	}
}
