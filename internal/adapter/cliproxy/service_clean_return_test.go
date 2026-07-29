package cliproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCleanRunReturnBeforeReadinessFailsFast proves that an SDK Run returning
// nil before readiness is reported as an error instead of being mistaken for a
// successful startup.
func TestCleanRunReturnBeforeReadinessFailsFast(t *testing.T) {
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        func(context.Context) error { return nil },
		shutdown:   func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, make(chan struct{}), nil, time.Second)

	err := awaitRunResult(t, service, context.Background())
	if !errors.Is(err, ErrSDKUnexpectedReturn) {
		t.Fatalf("Run error = %v, want %v", err, ErrSDKUnexpectedReturn)
	}
	if calls := proxy.shutdownCalls.Load(); calls != 0 {
		t.Fatalf("shutdown calls = %d, want 0", calls)
	}
}

// TestCleanRunReturnAfterReadinessDrainsAndFails proves that a clean return
// observed after readiness still drains outstanding usage groups and reports an
// error rather than a normal stop.
func TestCleanRunReturnAfterReadinessDrainsAndFails(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	requestID := "6c4d5f0e-0d0f-4a2f-8d51-2c1a2f7e5b90"
	if !bridge.reserve(requestID) {
		t.Fatal("reserve failed")
	}
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        func(context.Context) error { return nil },
		shutdown:   func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, 200*time.Millisecond)
	service.usageBridge = bridge

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(context.Background()) }()

	assertRunStillBlocked(t, runDone)
	if !bridge.release(requestID) {
		t.Fatal("release failed")
	}

	err := awaitRunDone(t, runDone)
	if !errors.Is(err, ErrSDKUnexpectedReturn) {
		t.Fatalf("Run error = %v, want %v", err, ErrSDKUnexpectedReturn)
	}
	if outstanding := bridge.outstanding(); outstanding != 0 {
		t.Fatalf("outstanding usage groups = %d, want 0", outstanding)
	}
}

// awaitRunResult runs the service and returns its bounded result.
func awaitRunResult(t *testing.T, service *Service, ctx context.Context) error {
	t.Helper()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	return awaitRunDone(t, runDone)
}

// awaitRunDone fails the test instead of blocking forever on a stuck lifecycle.
func awaitRunDone(t *testing.T, runDone <-chan error) error {
	t.Helper()
	select {
	case err := <-runDone:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return: the lifecycle is stuck")
		return nil
	}
}
