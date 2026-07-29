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

// TestCleanRunReturnDuringReadinessRaceIsAlwaysAnError proves that whichever
// side of the readiness/clean-return race is observed first, the unrequested
// stop is reported identically and the one-shot value is never read twice.
func TestCleanRunReturnDuringReadinessRaceIsAlwaysAnError(t *testing.T) {
	for attempt := range 50 {
		started := make(chan struct{})
		proxy := &fakeLifecycle{
			runEntered: make(chan struct{}),
			run: func(context.Context) error {
				close(started)
				return nil
			},
			shutdown: func(context.Context) error { return nil },
		}
		service := newLifecycleService(proxy, started, nil, 200*time.Millisecond)

		err := awaitRunResult(t, service, context.Background())
		if !errors.Is(err, ErrSDKUnexpectedReturn) {
			t.Fatalf("attempt %d: Run error = %v, want %v", attempt, err, ErrSDKUnexpectedReturn)
		}
	}
}

// TestCancellationAfterCleanRunReturnNeverHangs proves that cancelling after
// the clean-return race cannot wait for a lifecycle value that was already
// consumed. An explicit stop may legitimately succeed, so only termination is
// asserted here.
func TestCancellationAfterCleanRunReturnNeverHangs(t *testing.T) {
	for attempt := range 50 {
		started := make(chan struct{})
		proxy := &fakeLifecycle{
			runEntered: make(chan struct{}),
			run: func(context.Context) error {
				close(started)
				return nil
			},
			shutdown: func(context.Context) error { return nil },
		}
		service := newLifecycleService(proxy, started, nil, 200*time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- service.Run(ctx) }()
		cancel()

		awaitRunDone(t, runDone)
		if err := service.Close(); !errors.Is(err, service.result()) {
			t.Fatalf("attempt %d: Close error = %v, want the stable Run result", attempt, err)
		}
	}
}

// TestCleanRunReturnAfterStopRequestNeverHangs proves that a concurrent Close
// cannot combine with a clean SDK return to block Run or Close forever.
func TestCleanRunReturnAfterStopRequestNeverHangs(t *testing.T) {
	release := make(chan struct{})
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run: func(context.Context) error {
			<-release
			return nil
		},
		shutdown: func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, 200*time.Millisecond)

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(context.Background()) }()
	<-proxy.runEntered

	closeDone := make(chan error, 1)
	go func() { closeDone <- service.Close() }()
	close(release)

	runErr := awaitRunDone(t, runDone)
	if closeErr := awaitRunDone(t, closeDone); !errors.Is(closeErr, runErr) {
		t.Fatalf("Close error = %v, want the stable Run result %v", closeErr, runErr)
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
