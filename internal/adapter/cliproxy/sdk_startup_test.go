package cliproxy

import (
	"runtime"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestSDKStartupBarrierRequiresExactLifecycleEvent rejects similar application logs.
func TestSDKStartupBarrierRequiresExactLifecycleEvent(t *testing.T) {
	ready := make(chan struct{})
	barrier := &sdkStartupBarrier{}
	barrier.arm(&Service{}, ready)

	if err := barrier.Fire(&log.Entry{Message: "file watcher started"}); err != nil {
		t.Fatal(err)
	}
	assertNoStartupSignal(t, ready)
	if err := barrier.Fire(&log.Entry{Message: sdkRuntimeReadyMessage + " from application"}); err != nil {
		t.Fatal(err)
	}
	assertNoStartupSignal(t, ready)
	if err := barrier.Fire(&log.Entry{
		Message: sdkRuntimeReadyMessage,
		Caller:  &runtime.Frame{Function: "application.main"},
	}); err != nil {
		t.Fatal(err)
	}
	assertNoStartupSignal(t, ready)
	if err := barrier.Fire(&log.Entry{Message: "core auth auto-refresh started (interval=15m0s)"}); err != nil {
		t.Fatal(err)
	}
	assertStartupSignal(t, ready)
	if err := barrier.Fire(&log.Entry{Message: sdkRuntimeReadyMessage}); err != nil {
		t.Fatal(err)
	}
}

// TestSDKStartupBarrierDisarmIgnoresLaterLifecycleEvent verifies cleanup isolation.
func TestSDKStartupBarrierDisarmIgnoresLaterLifecycleEvent(t *testing.T) {
	ready := make(chan struct{})
	barrier := &sdkStartupBarrier{}
	owner := &Service{}
	barrier.arm(owner, ready)
	barrier.disarm(owner, ready)

	if err := barrier.Fire(&log.Entry{Message: sdkRuntimeReadyMessage}); err != nil {
		t.Fatal(err)
	}
	assertNoStartupSignal(t, ready)
}

// assertNoStartupSignal verifies the compatibility barrier remains closed.
func assertNoStartupSignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
		t.Fatal("SDK startup signal arrived early or more than once")
	default:
	}
}

// assertStartupSignal verifies the compatibility barrier opens.
func assertStartupSignal(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	default:
		t.Fatal("SDK startup signal is absent")
	}
}
