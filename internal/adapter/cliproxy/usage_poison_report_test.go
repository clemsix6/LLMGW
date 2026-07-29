package cliproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestPoisonIsReportedExactlyOnce proves the terminal usage state is surfaced to
// the composition root, so the process can stop instead of serving only 503.
func TestPoisonIsReportedExactlyOnce(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	reports := make(chan struct{}, 4)
	bridge.ReportPoisonWith(func() { reports <- struct{}{} })

	bridge.poison()
	bridge.poison()

	select {
	case <-reports:
	case <-time.After(time.Second):
		t.Fatal("poison was never reported")
	}
	select {
	case <-reports:
		t.Fatal("poison was reported more than once")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPoisonWithoutObserverDoesNotPanic keeps the bridge usable in tests and in
// any composition that does not install an observer.
func TestPoisonWithoutObserverDoesNotPanic(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	bridge.poison()
	if !bridge.poisoned() {
		t.Fatal("bridge is not poisoned")
	}
}

// TestHealthRouteReportsPoisonedBridge proves the health route stops advertising
// a healthy gateway once no generation can be admitted any more.
func TestHealthRouteReportsPoisonedBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bridge := fixedUsageBridgeCapacity(t, 1)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge)

	engine := gin.New()
	engine.Use(middleware.Handler())
	engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })

	healthy := httptest.NewRecorder()
	engine.ServeHTTP(healthy, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthy.Code != http.StatusOK {
		t.Fatalf("healthy status = %d, want %d", healthy.Code, http.StatusOK)
	}

	bridge.poison()

	poisoned := httptest.NewRecorder()
	engine.ServeHTTP(poisoned, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if poisoned.Code != http.StatusServiceUnavailable {
		t.Fatalf("poisoned status = %d, want %d", poisoned.Code, http.StatusServiceUnavailable)
	}
}
