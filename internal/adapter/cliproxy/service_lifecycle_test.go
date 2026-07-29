package cliproxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// TestServiceCancellationUsesFreshShutdownAndWaitsForActiveWork verifies shutdown timing and order.
func TestServiceCancellationUsesFreshShutdownAndWaitsForActiveWork(t *testing.T) {
	started := closedSignal()
	shutdownEntered := make(chan time.Duration, 1)
	releaseActiveWork := make(chan struct{})
	var clearCalls atomic.Int64

	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("shutdown context has no deadline")
			}
			shutdownEntered <- time.Until(deadline)
			select {
			case <-releaseActiveWork:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	service := newLifecycleService(proxy, started, func() { clearCalls.Add(1) }, 80*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()

	<-proxy.runEntered
	time.Sleep(120 * time.Millisecond)
	cancel()
	if remaining := <-shutdownEntered; remaining < 50*time.Millisecond {
		t.Fatalf("fresh shutdown deadline remaining = %s, want at least 50ms", remaining)
	}
	assertRunStillBlocked(t, runDone)
	if got := clearCalls.Load(); got != 0 {
		t.Fatalf("access cleared before active work drained: %d", got)
	}

	close(releaseActiveWork)
	if err := <-runDone; err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if got := clearCalls.Load(); got != 1 {
		t.Fatalf("access clear calls = %d, want 1", got)
	}
}

func TestServiceShutdownWaitsForUsageBarrierDrain(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	requestID := "f5efc3a8-e6c3-49fd-bad6-6532fa51d216"
	if !bridge.reserve(requestID) {
		t.Fatal("reserve failed")
	}
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown:   func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, time.Second)
	service.usageBridge = bridge
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	<-proxy.runEntered

	cancel()
	assertRunStillBlocked(t, runDone)
	if !bridge.release(requestID) {
		t.Fatal("release failed")
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Run error after usage drain = %v", err)
	}
}

// TestServiceShutdownErrorStillWaitsForUsageDrain catches an SDK shutdown
// failure bypassing the callback barrier before workers and PostgreSQL close.
func TestServiceShutdownErrorStillWaitsForUsageDrain(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	requestID := "ef552a85-234c-430f-9fc2-b4e4a92cf776"
	if !bridge.reserve(requestID) {
		t.Fatal("reserve failed")
	}
	shutdownErr := errors.New("listener shutdown failed")
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown:   func(context.Context) error { return shutdownErr },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, time.Second)
	service.usageBridge = bridge
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	<-proxy.runEntered

	cancel()
	assertRunStillBlocked(t, runDone)
	if !bridge.release(requestID) {
		t.Fatal("release failed")
	}
	if err := <-runDone; !errors.Is(err, shutdownErr) {
		t.Fatalf("Run error after failed shutdown/drain = %v, want shutdown cause", err)
	}
}

// TestServiceNaturalReturnWaitsForUsageDrain catches the natural SDK-return
// branch bypassing the real usage bridge and returning while a reserved group
// can still own queued callbacks.
func TestServiceNaturalReturnWaitsForUsageDrain(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	requestID := "c22a569b-5b2d-48c6-882f-3a57a9eef4be"
	if !bridge.reserve(requestID) {
		t.Fatal("reserve failed")
	}
	returnSDK := make(chan struct{})
	runErr := errors.New("listener stopped")
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run: func(context.Context) error {
			<-returnSDK
			return runErr
		},
		shutdown: func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, time.Second)
	service.usageBridge = bridge
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(context.Background()) }()
	<-proxy.runEntered

	close(returnSDK)
	assertRunStillBlocked(t, runDone)
	if !bridge.release(requestID) {
		t.Fatal("release failed")
	}
	if err := <-runDone; !errors.Is(err, runErr) {
		t.Fatalf("natural Run error = %v, want listener cause", err)
	}
	if got := proxy.shutdownCalls.Load(); got != 0 {
		t.Fatalf("natural-return Shutdown calls = %d, want 0", got)
	}
}

// TestServiceNaturalReturnJoinsDrainTimeout catches replacing errors.Join with
// either the original SDK error or the fresh bounded usage-drain failure.
func TestServiceNaturalReturnJoinsDrainTimeout(t *testing.T) {
	bridge := fixedUsageBridgeCapacity(t, 1)
	if !bridge.reserve("367ab6aa-afd0-4948-b472-e825170177d8") {
		t.Fatal("reserve failed")
	}
	runErr := errors.New("listener stopped")
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        func(context.Context) error { return runErr },
		shutdown:   func(context.Context) error { return nil },
	}
	service := newLifecycleService(proxy, closedSignal(), nil, 30*time.Millisecond)
	service.usageBridge = bridge
	startedAt := time.Now()

	err := service.Run(context.Background())

	if !errors.Is(err, runErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("natural Run error = %v, want joined listener/deadline causes", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 20*time.Millisecond {
		t.Fatalf("fresh drain elapsed = %s, want bounded wait", elapsed)
	}
}

// TestServiceEarlyCancellationWaitsForStartupBarrier verifies cancellation is remembered safely.
func TestServiceEarlyCancellationWaitsForStartupBarrier(t *testing.T) {
	started := make(chan struct{})
	shutdownEntered := make(chan struct{}, 1)
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown: func(context.Context) error {
			shutdownEntered <- struct{}{}
			return nil
		},
	}
	service := newLifecycleService(proxy, started, nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	<-proxy.runEntered

	select {
	case <-shutdownEntered:
		t.Fatal("Shutdown started before the SDK startup barrier")
	default:
	}
	close(started)
	awaitLifecycleSignal(t, shutdownEntered)
	if err := <-runDone; err != nil {
		t.Fatalf("Run error = %v", err)
	}
}

// TestServiceRejectsFilteredStartupEventBeforeProxyRun verifies Logrus preflight.
func TestServiceRejectsFilteredStartupEventBeforeProxyRun(t *testing.T) {
	previousLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	defer log.SetLevel(previousLevel)

	proxy := newBlockingFakeLifecycle()
	var clearCalls atomic.Int64
	service := newLifecycleService(proxy, closedSignal(), func() { clearCalls.Add(1) }, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := service.Run(ctx)
	if !errors.Is(err, ErrSDKStartupCompatibility) {
		t.Fatalf("Run error = %v, want startup compatibility cause", err)
	}
	if !strings.Contains(err.Error(), "Logrus Info") {
		t.Fatalf("Run error does not identify filtered Logrus Info: %v", err)
	}
	if got := proxy.runCalls.Load(); got != 0 {
		t.Fatalf("proxy Run calls = %d, want 0", got)
	}
	if got := clearCalls.Load(); got != 1 {
		t.Fatalf("access clear calls = %d, want 1", got)
	}
}

// TestServiceStartupCleanupTimeoutRetainsFailClosedIsolation verifies bounded failure.
func TestServiceStartupCleanupTimeoutRetainsFailClosedIsolation(t *testing.T) {
	releaseSDK := make(chan struct{})
	proxy := newBlockingFakeLifecycle()
	proxy.run = func(context.Context) error {
		<-releaseSDK
		return nil
	}
	var clearCalls atomic.Int64
	cleared := make(chan struct{})
	service := newLifecycleService(proxy, make(chan struct{}), func() {
		clearCalls.Add(1)
		close(cleared)
	}, 30*time.Millisecond)
	service.startupTimeout = 20 * time.Millisecond
	err := service.Run(context.Background())
	if !errors.Is(err, ErrSDKCleanupIncomplete) {
		t.Fatalf("Run error = %v, want incomplete cleanup cause", err)
	}
	if got := clearCalls.Load(); got != 0 {
		t.Fatalf("access clear calls after incomplete cleanup = %d, want 0", got)
	}
	if _, err := service.process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleConsumed) {
		t.Fatalf("construction after started lifecycle = %v, want consumed", err)
	}
	close(releaseSDK)
	awaitLifecycleSignal(t, cleared)
	if _, err := service.process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleConsumed) {
		t.Fatalf("construction after delayed SDK return = %v, want consumed", err)
	}
}

// TestServiceMissingStartupEventTimesOutAndCleansSDK verifies bounded fail-closed cleanup.
func TestServiceMissingStartupEventTimesOutAndCleansSDK(t *testing.T) {
	var sdkCleanupCalls atomic.Int64
	proxy := newBlockingFakeLifecycle()
	proxy.run = func(ctx context.Context) error {
		<-ctx.Done()
		sdkCleanupCalls.Add(1)
		return ctx.Err()
	}
	var clearCalls atomic.Int64
	service := newLifecycleService(proxy, make(chan struct{}), func() { clearCalls.Add(1) }, time.Second)
	service.startupTimeout = 30 * time.Millisecond

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(context.Background()) }()
	err := awaitLifecycleResult(t, runDone)
	if !errors.Is(err, ErrSDKStartupCompatibility) {
		t.Fatalf("Run error = %v, want startup compatibility cause", err)
	}
	if got := sdkCleanupCalls.Load(); got != 1 {
		t.Fatalf("SDK cleanup calls = %d, want 1", got)
	}
	if got := proxy.shutdownCalls.Load(); got != 0 {
		t.Fatalf("public Shutdown calls before readiness = %d, want 0", got)
	}
	if got := clearCalls.Load(); got != 1 {
		t.Fatalf("access clear calls = %d, want 1", got)
	}
}

// TestServiceRejectsConstructionBeforeAndAfterRun verifies process-wide
// construction exclusion and the permanent first-Run tombstone.
func TestServiceRejectsConstructionBeforeAndAfterRun(t *testing.T) {
	firstProxy := newBlockingFakeLifecycle()
	first := newLifecycleService(firstProxy, closedSignal(), nil, time.Second)
	if _, err := first.process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleActive) {
		t.Fatalf("concurrent construction error = %v, want active", err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(context.Background()) }()
	<-firstProxy.runEntered

	if _, err := first.process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleConsumed) {
		t.Fatalf("construction after Run error = %v, want consumed", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lifecycle: %v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first lifecycle Run error = %v", err)
	}
	if _, err := first.process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleConsumed) {
		t.Fatalf("construction after shutdown error = %v, want consumed", err)
	}
}

// TestServiceRunPropagatesFreshShutdownError verifies cancellation is normal only after shutdown.
func TestServiceRunPropagatesFreshShutdownError(t *testing.T) {
	shutdownErr := errors.New("active request did not drain")
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown:   func(context.Context) error { return shutdownErr },
	}
	var clearCalls atomic.Int64
	service := newLifecycleService(proxy, closedSignal(), func() { clearCalls.Add(1) }, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()

	<-proxy.runEntered
	cancel()
	err := <-runDone
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("Run error = %v, want shutdown cause", err)
	}
	if got := clearCalls.Load(); got != 1 {
		t.Fatalf("access clear calls = %d, want 1", got)
	}
}

// TestServiceRunIsOneShotAndCloseDuringRunIsIdempotent verifies race-safe lifecycle ownership.
func TestServiceRunIsOneShotAndCloseDuringRunIsIdempotent(t *testing.T) {
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown:   func(context.Context) error { return nil },
	}
	var clearCalls atomic.Int64
	service := newLifecycleService(proxy, closedSignal(), func() { clearCalls.Add(1) }, time.Second)
	firstRun := make(chan error, 1)
	go func() { firstRun <- service.Run(context.Background()) }()
	<-proxy.runEntered

	if err := service.Run(context.Background()); !errors.Is(err, ErrServiceAlreadyRun) {
		t.Fatalf("second Run error = %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close during Run error = %v", err)
	}
	if err := <-firstRun; err != nil {
		t.Fatalf("first Run error = %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("second Close error = %v", err)
	}
	if got := proxy.runCalls.Load(); got != 1 {
		t.Fatalf("proxy Run calls = %d, want 1", got)
	}
	if got := proxy.shutdownCalls.Load(); got != 1 {
		t.Fatalf("proxy Shutdown calls = %d, want 1", got)
	}
	if got := clearCalls.Load(); got != 1 {
		t.Fatalf("access clear calls = %d, want 1", got)
	}
}

// TestServiceCloseBeforeRunCleansConstruction verifies cleanup without consuming a Run.
func TestServiceCloseBeforeRunCleansConstruction(t *testing.T) {
	proxy := &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown:   func(context.Context) error { return nil },
	}
	var clearCalls atomic.Int64
	service := newLifecycleService(proxy, closedSignal(), func() { clearCalls.Add(1) }, time.Second)

	if err := service.Close(); err != nil {
		t.Fatalf("first Close error = %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("second Close error = %v", err)
	}
	if err := service.Run(context.Background()); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("Run after Close error = %v", err)
	}
	if got := proxy.runCalls.Load(); got != 0 {
		t.Fatalf("proxy Run calls = %d, want 0", got)
	}
	if got := clearCalls.Load(); got != 1 {
		t.Fatalf("access clear calls = %d, want 1", got)
	}
}

// assertRunStillBlocked verifies active work prevents lifecycle return.
func assertRunStillBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Run returned before active work drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
}

// awaitLifecycleSignal waits for one bounded unit-test lifecycle event.
func awaitLifecycleSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("lifecycle signal did not arrive")
	}
}

// awaitLifecycleResult waits for a bounded unit-test lifecycle return.
func awaitLifecycleResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("lifecycle did not return before test timeout")
		return nil
	}
}

// closedSignal returns an already-closed startup signal.
func closedSignal() <-chan struct{} {
	signal := make(chan struct{})
	close(signal)
	return signal
}

// waitForCancellation models the blocking portion of SDK Run.
func waitForCancellation(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// newBlockingFakeLifecycle creates a fake SDK that exits on context cancellation.
func newBlockingFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{
		runEntered: make(chan struct{}),
		run:        waitForCancellation,
		shutdown:   func(context.Context) error { return nil },
	}
}

// fakeLifecycle models the public SDK Run and Shutdown methods.
type fakeLifecycle struct {
	run      func(context.Context) error // run implements the fake SDK Run.
	shutdown func(context.Context) error // shutdown implements the fake SDK Shutdown.

	runEntered    chan struct{} // runEntered closes when the first Run begins.
	runEnterOnce  sync.Once     // runEnterOnce protects runEntered.
	runCalls      atomic.Int64  // runCalls counts SDK Run calls.
	shutdownCalls atomic.Int64  // shutdownCalls counts SDK Shutdown calls.
}

// Run records and delegates one fake SDK lifecycle.
func (f *fakeLifecycle) Run(ctx context.Context) error {
	f.runCalls.Add(1)
	f.runEnterOnce.Do(func() {
		close(f.runEntered)
	})
	return f.run(ctx)
}

// Shutdown records and delegates one fake SDK shutdown.
func (f *fakeLifecycle) Shutdown(ctx context.Context) error {
	f.shutdownCalls.Add(1)
	return f.shutdown(ctx)
}
