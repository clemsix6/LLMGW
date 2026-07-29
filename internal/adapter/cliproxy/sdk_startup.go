package cliproxy

import (
	"sync"

	log "github.com/sirupsen/logrus"
)

const (
	sdkRuntimeReadyMessage = "core auth auto-refresh started (interval=15m0s)"
	sdkRuntimeReadyCaller  = "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy.(*Service).Run"
)

var (
	sharedSDKStartupBarrier = &sdkStartupBarrier{}
	sdkStartupHookOnce      sync.Once
)

// sdkStartupBarrier observes the pinned SDK's final synchronous initialization event.
//
// CLIProxyAPI v7.2.102 invokes OnAfterStart before assigning fields consumed by
// Shutdown. Its final initialization log occurs after those assignments and
// the core auto-refresh state, so this hook supplies the missing happens-before
// edge for concurrent Run and Shutdown.
type sdkStartupBarrier struct {
	mu    sync.Mutex    // mu protects the sole process-wide startup waiter.
	owner *Service      // owner identifies the reserved wrapper lifecycle.
	ready chan struct{} // ready closes after complete SDK initialization.
}

// armSDKStartupBarrier installs one hook and records the sole SDK startup waiter.
func armSDKStartupBarrier(owner *Service, ready chan struct{}) {
	sdkStartupHookOnce.Do(func() {
		log.AddHook(sharedSDKStartupBarrier)
	})
	if ownsSDKLifecycle(owner) {
		sharedSDKStartupBarrier.arm(owner, ready)
	}
}

// arm records a waiter without replacing an active process-wide SDK startup.
func (b *sdkStartupBarrier) arm(owner *Service, ready chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready == nil {
		b.owner = owner
		b.ready = ready
	}
}

// disarmSDKStartupBarrier removes a matching waiter after startup failure or cleanup.
func disarmSDKStartupBarrier(owner *Service, ready <-chan struct{}) {
	sharedSDKStartupBarrier.disarm(owner, ready)
}

// disarm removes only the matching lifecycle waiter.
func (b *sdkStartupBarrier) disarm(owner *Service, ready <-chan struct{}) {
	b.mu.Lock()
	if b.owner == owner && b.ready == ready {
		b.owner = nil
		b.ready = nil
	}
	b.mu.Unlock()
}

// Levels restricts the observer to the SDK's informational startup event.
func (b *sdkStartupBarrier) Levels() []log.Level {
	return []log.Level{log.InfoLevel}
}

// Fire emits readiness after the SDK's last synchronous initialization event.
func (b *sdkStartupBarrier) Fire(entry *log.Entry) error {
	if b == nil || entry == nil {
		return nil
	}
	if entry.Message != sdkRuntimeReadyMessage {
		return nil
	}
	// Logrus omits Caller unless the application enables ReportCaller. When
	// available, reject an exact application-authored message too. With the
	// default nil Caller, exact matching plus the reserved lifecycle and narrow
	// OnAfterStart arm window are the strongest origin proof Logrus exposes.
	if entry.Caller != nil && entry.Caller.Function != sdkRuntimeReadyCaller {
		return nil
	}
	b.mu.Lock()
	if b.ready != nil {
		close(b.ready)
		b.owner = nil
		b.ready = nil
	}
	b.mu.Unlock()
	return nil
}
