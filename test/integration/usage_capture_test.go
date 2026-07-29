package integration

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// usageObservation contains only the safe SDK principal.
type usageObservation struct {
	Principal   string    // Principal is the SDK userApiKey value.
	RequestedAt time.Time // RequestedAt is the pinned SDK attempt start time.
}

// usageCapture records safe usage principal fields for integration assertions.
type usageCapture struct {
	observations chan usageObservation // observations receives bounded usage events.
}

// newUsageCapture creates a non-blocking usage observer.
func newUsageCapture() *usageCapture {
	return &usageCapture{observations: make(chan usageObservation, 64)}
}

// HandleUsage captures only the opaque principal and discards all payload metadata.
func (c *usageCapture) HandleUsage(_ context.Context, record sdkusage.Record) {
	select {
	case c.observations <- usageObservation{
		Principal:   record.APIKey,
		RequestedAt: record.RequestedAt,
	}:
	default:
	}
}

// awaitObservation waits for the next immutable SDK usage principal.
func (c *usageCapture) awaitObservation(t *testing.T) usageObservation {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case observation := <-c.observations:
		return observation
	case <-timer.C:
		t.Fatal("SDK usage event was not observed")
	}
	return usageObservation{}
}

// lockedBuffer is a concurrency-safe log destination.
type lockedBuffer struct {
	mu     sync.Mutex   // mu protects buffer.
	buffer bytes.Buffer // buffer retains logs for leak assertions.
}

// Write appends one log fragment.
func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

// Contains reports whether captured logs contain value.
func (b *lockedBuffer) Contains(value string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buffer.String(), value)
}
