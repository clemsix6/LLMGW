package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	serviceShutdownTimeout = 30 * time.Second
	serviceStartupTimeout  = 5 * time.Second
)

var (
	// ErrServiceAlreadyRun reports a second or concurrent Run call.
	ErrServiceAlreadyRun = errors.New("embedded CLIProxyAPI service Run is one-shot")
	// ErrServiceClosed reports Run after construction cleanup.
	ErrServiceClosed = errors.New("embedded CLIProxyAPI service is closed")
	// ErrSDKStartupCompatibility reports a failed pinned-SDK readiness contract.
	ErrSDKStartupCompatibility = errors.New("CLIProxyAPI startup compatibility check failed")
	// ErrSDKLifecycleActive reports another process-wide SDK lifecycle.
	ErrSDKLifecycleActive = errors.New("another embedded CLIProxyAPI lifecycle is active")
	// ErrSDKLifecycleConsumed reports that this process already started the
	// SDK default usage manager, which cannot be reopened after Shutdown.
	ErrSDKLifecycleConsumed = errors.New("embedded CLIProxyAPI lifecycle is already consumed")
	// ErrSDKCleanupIncomplete reports that bounded fail-closed cleanup did not return.
	ErrSDKCleanupIncomplete = errors.New("embedded CLIProxyAPI cleanup did not return")
	// ErrSDKUnexpectedReturn reports that SDK Run stopped serving on its own.
	// Only an explicit shutdown may stop the SDK without an error.
	ErrSDKUnexpectedReturn = errors.New("embedded CLIProxyAPI stopped serving unexpectedly")
)

type sdkProcessPhase uint8

const (
	sdkProcessIdle sdkProcessPhase = iota
	sdkProcessConstructed
	sdkProcessConsumed
)

type sdkConstructionLease struct{}

// sdkProcessState serializes construction and permanently tombstones the
// process as soon as the SDK/default usage manager can be touched by Run.
type sdkProcessState struct {
	mu    sync.Mutex
	phase sdkProcessPhase
	lease *sdkConstructionLease
}

var defaultSDKProcessState = newSDKProcessState()

func newSDKProcessState() *sdkProcessState {
	return &sdkProcessState{}
}

func (s *sdkProcessState) reserveConstruction() (*sdkConstructionLease, error) {
	if s == nil {
		return nil, ErrSDKLifecycleConsumed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case sdkProcessConstructed:
		return nil, ErrSDKLifecycleActive
	case sdkProcessConsumed:
		return nil, ErrSDKLifecycleConsumed
	}
	lease := &sdkConstructionLease{}
	s.phase = sdkProcessConstructed
	s.lease = lease
	return lease, nil
}

func (s *sdkProcessState) consume(lease *sdkConstructionLease) error {
	if s == nil || lease == nil {
		return ErrSDKLifecycleConsumed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == sdkProcessConsumed {
		return ErrSDKLifecycleConsumed
	}
	if s.phase != sdkProcessConstructed || s.lease != lease {
		return ErrSDKLifecycleActive
	}
	s.phase = sdkProcessConsumed
	return nil
}

func (s *sdkProcessState) releaseConstruction(lease *sdkConstructionLease) {
	if s == nil || lease == nil {
		return
	}
	s.mu.Lock()
	if s.phase == sdkProcessConstructed && s.lease == lease {
		s.phase = sdkProcessIdle
		s.lease = nil
	}
	s.mu.Unlock()
}

// proxyLifecycle is the public SDK lifecycle surface used by Service.
type proxyLifecycle interface {
	// Run starts the SDK and blocks until cancellation or listener return.
	Run(context.Context) error
	// Shutdown stops the listener and active SDK components.
	Shutdown(context.Context) error
}

// Service owns one embedded CLIProxyAPI lifecycle and its access registration.
type Service struct {
	proxy           proxyLifecycle        // proxy is the secured SDK service.
	started         <-chan struct{}       // started closes after complete SDK initialization.
	clearAccess     func()                // clearAccess removes exclusive global access.
	usageBridge     *UsageBridge          // usageBridge proves SDK record groups drained.
	startup         *sdkStartupSnapshot   // startup owns frozen config and auth files.
	process         *sdkProcessState      // process owns construction/consumption.
	lease           *sdkConstructionLease // lease is releasable only before Run.
	registerUsage   func()                // registerUsage installs plugins at first Run.
	shutdownTimeout time.Duration         // shutdownTimeout bounds fresh shutdown work.
	startupTimeout  time.Duration         // startupTimeout bounds the pinned-SDK barrier.

	mu           sync.Mutex    // mu protects lifecycle flags and result.
	runCalled    bool          // runCalled records the one permitted Run.
	closed       bool          // closed records cleanup before Run.
	runErr       error         // runErr is the stable Run and Close result.
	runDone      chan struct{} // runDone closes after shutdown and cleanup.
	stopRequest  chan struct{} // stopRequest asks Run to initiate shutdown.
	stopOnce     sync.Once     // stopOnce closes stopRequest once.
	finishOnce   sync.Once     // finishOnce stores the result and closes runDone.
	clearOnce    sync.Once     // clearOnce removes access exactly once.
	registerOnce sync.Once     // registerOnce installs global usage plugins once.
}

// newLifecycleService constructs the state machine around one SDK service.
func newLifecycleService(
	proxy proxyLifecycle,
	started <-chan struct{},
	clearAccess func(),
	shutdownTimeout time.Duration,
) *Service {
	process := newSDKProcessState()
	lease, _ := process.reserveConstruction()
	return newLifecycleServiceWithLease(
		proxy,
		started,
		clearAccess,
		shutdownTimeout,
		process,
		lease,
	)
}

func newLifecycleServiceWithLease(
	proxy proxyLifecycle,
	started <-chan struct{},
	clearAccess func(),
	shutdownTimeout time.Duration,
	process *sdkProcessState,
	lease *sdkConstructionLease,
) *Service {
	return &Service{
		proxy:           proxy,
		started:         started,
		clearAccess:     clearAccess,
		process:         process,
		lease:           lease,
		shutdownTimeout: shutdownTimeout,
		startupTimeout:  serviceStartupTimeout,
		runDone:         make(chan struct{}),
		stopRequest:     make(chan struct{}),
	}
}

// Run starts the SDK exactly once and owns its complete shutdown lifecycle.
func (s *Service) Run(ctx context.Context) error {
	if err := s.beginRun(); err != nil {
		return err
	}
	s.registerOnce.Do(func() {
		if s.registerUsage != nil {
			s.registerUsage()
		}
	})
	if err := validateSDKStartupLogLevel(); err != nil {
		s.finish(err)
		return s.result()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	proxyCtx, cancelProxy := context.WithCancel(context.Background())
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- s.proxy.Run(proxyCtx)
	}()

	runErr := s.waitForStartup(ctx, proxyDone, cancelProxy)
	s.finish(runErr)
	return s.result()
}

// Close cleans an unstarted service or requests and waits for the active Run.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.runCalled {
		if s.closed {
			done := s.runDone
			s.mu.Unlock()
			<-done
			return s.result()
		}
		s.closed = true
		s.mu.Unlock()
		s.finish(nil)
		if s.result() == nil {
			s.process.releaseConstruction(s.lease)
		}
		return s.result()
	}
	done := s.runDone
	s.mu.Unlock()

	s.stopOnce.Do(func() { close(s.stopRequest) })
	<-done
	return s.result()
}

// beginRun atomically admits the sole Run call.
func (s *Service) beginRun() error {
	if s == nil || s.proxy == nil {
		return errors.New("run embedded CLIProxyAPI service:\nservice is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	if s.runCalled {
		return ErrServiceAlreadyRun
	}
	if err := s.process.consume(s.lease); err != nil {
		return err
	}
	s.runCalled = true
	return nil
}

// validateSDKStartupLogLevel rejects a readiness event Logrus would filter.
func validateSDKStartupLogLevel() error {
	if log.StandardLogger().IsLevelEnabled(log.InfoLevel) {
		return nil
	}
	cause := errors.Join(
		ErrSDKStartupCompatibility,
		errors.New("required Logrus Info event is filtered"),
	)
	return fmt.Errorf(
		"establish embedded CLIProxyAPI startup readiness:\n%w",
		cause,
	)
}

// waitForStartup remembers stop requests while bounding readiness.
func (s *Service) waitForStartup(
	ctx context.Context,
	proxyDone <-chan error,
	cancelProxy context.CancelFunc,
) error {
	timeout := s.startupTimeout
	if timeout <= 0 {
		timeout = serviceStartupTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ctxDone := ctx.Done()
	var closeRequest <-chan struct{} = s.stopRequest
	stopRequested := false
	for {
		runErr, complete := s.observeStartup(
			&ctxDone,
			&closeRequest,
			&stopRequested,
			proxyDone,
			timer.C,
		)
		if !complete {
			continue
		}
		return s.completeStartup(ctx, runErr, stopRequested, proxyDone, cancelProxy)
	}
}

// completeStartup selects failure cleanup, immediate stop, or normal serving.
func (s *Service) completeStartup(
	ctx context.Context,
	runErr error,
	stopRequested bool,
	proxyDone <-chan error,
	cancelProxy context.CancelFunc,
) error {
	if errors.Is(runErr, ErrSDKStartupCompatibility) {
		return s.cleanupFailedStartup(runErr, proxyDone, cancelProxy)
	}
	if runErr != nil {
		return runErr
	}
	if stopRequested {
		return s.shutdownStarted(proxyDone, cancelProxy)
	}
	return s.waitForStop(ctx, proxyDone, cancelProxy)
}

// observeStartup consumes one readiness, lifecycle, timeout, or stop event.
func (s *Service) observeStartup(
	ctxDone *<-chan struct{},
	closeRequest *<-chan struct{},
	stopRequested *bool,
	proxyDone <-chan error,
	timeout <-chan time.Time,
) (error, bool) {
	select {
	case runErr := <-proxyDone:
		select {
		case <-s.started:
			return s.naturalRunError(runErr), true
		default:
		}
		return earlyRunError(runErr), true
	case <-s.started:
		return nil, true
	case <-timeout:
		return startupCompatibilityError(), true
	case <-*ctxDone:
		*stopRequested = true
		*ctxDone = nil
		return nil, false
	case <-*closeRequest:
		*stopRequested = true
		*closeRequest = nil
		return nil, false
	}
}

// waitForStop observes natural return, caller cancellation, or Close.
func (s *Service) waitForStop(
	ctx context.Context,
	proxyDone <-chan error,
	cancelProxy context.CancelFunc,
) error {
	select {
	case runErr := <-proxyDone:
		return s.naturalRunError(runErr)
	case <-ctx.Done():
		return s.shutdownStarted(proxyDone, cancelProxy)
	case <-s.stopRequest:
		return s.shutdownStarted(proxyDone, cancelProxy)
	}
}

// naturalRunError drains callbacks already accepted by an SDK dispatcher that
// has returned after closing its queue but before joining that dispatcher.
func (s *Service) naturalRunError(runErr error) error {
	drainErr := s.drainUsageFresh()
	return errors.Join(earlyRunError(runErr), drainErr)
}

// earlyRunError reports every SDK Run return observed outside explicit
// shutdown. It never returns nil, so a consumed one-shot lifecycle value can
// never be mistaken for readiness and read a second time.
func earlyRunError(runErr error) error {
	if wrapped := wrapRunError(runErr); wrapped != nil {
		return wrapped
	}
	return fmt.Errorf("run embedded CLIProxyAPI service:\n%w", ErrSDKUnexpectedReturn)
}

// shutdownStarted uses a fresh deadline after startup is established.
func (s *Service) shutdownStarted(
	proxyDone <-chan error,
	cancelProxy context.CancelFunc,
) error {
	shutdownErr := s.shutdownFresh()
	cancelProxy()
	runErr := <-proxyDone
	return stoppedRunError(runErr, shutdownErr)
}

// cleanupFailedStartup cancels SDK Run while its own shutdown deadline is fresh.
func (s *Service) cleanupFailedStartup(
	startupErr error,
	proxyDone <-chan error,
	cancelProxy context.CancelFunc,
) error {
	cancelProxy()
	timeout := s.shutdownTimeout
	if timeout <= 0 {
		timeout = serviceShutdownTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case runErr := <-proxyDone:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			return errors.Join(startupErr, wrapRunError(runErr))
		}
		return startupErr
	case <-timer.C:
		go s.releaseAfterDelayedSDKReturn(proxyDone)
		return errors.Join(startupErr, ErrSDKCleanupIncomplete)
	}
}

// releaseAfterDelayedSDKReturn retains isolation until the SDK eventually exits.
func (s *Service) releaseAfterDelayedSDKReturn(proxyDone <-chan error) {
	<-proxyDone
	if s.clearAccess != nil {
		s.clearOnce.Do(s.clearAccess)
	}
}

// startupCompatibilityError reports a missing pinned-SDK readiness event.
func startupCompatibilityError() error {
	cause := errors.Join(
		ErrSDKStartupCompatibility,
		errors.New("required exact startup event was not observed"),
	)
	return fmt.Errorf(
		"wait for embedded CLIProxyAPI v7.2.102 startup event:\n%w",
		cause,
	)
}

// shutdownFresh invokes public SDK shutdown with a newly-created timeout.
func (s *Service) shutdownFresh() error {
	timeout := s.shutdownTimeout
	if timeout <= 0 {
		timeout = serviceShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := s.proxy.Shutdown(ctx)
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("shutdown embedded CLIProxyAPI service:\n%w", shutdownErr)
	}
	return errors.Join(shutdownErr, s.drainUsage(ctx))
}

// drainUsageFresh gives natural SDK return a deadline independent of the caller.
func (s *Service) drainUsageFresh() error {
	timeout := s.shutdownTimeout
	if timeout <= 0 {
		timeout = serviceShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.drainUsage(ctx)
}

func (s *Service) drainUsage(ctx context.Context) error {
	if s.usageBridge == nil {
		return nil
	}
	if err := s.usageBridge.waitDrained(ctx); err != nil {
		return fmt.Errorf("drain embedded CLIProxyAPI usage groups:\n%w", err)
	}
	return nil
}

// stoppedRunError normalizes cancellation only when explicit shutdown succeeded.
func stoppedRunError(runErr error, shutdownErr error) error {
	if shutdownErr != nil {
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			return errors.Join(shutdownErr, wrapRunError(runErr))
		}
		return shutdownErr
	}
	if runErr == nil || errors.Is(runErr, context.Canceled) {
		return nil
	}
	return wrapRunError(runErr)
}

// wrapRunError adds lifecycle context without normalizing cancellation.
func wrapRunError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("run embedded CLIProxyAPI service:\n%w", err)
}

// finish stores the stable result and clears access after lifecycle return.
func (s *Service) finish(err error) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		s.runErr = err
		s.mu.Unlock()
		disarmSDKStartupBarrier(s, s.started)
		if errors.Is(err, ErrSDKCleanupIncomplete) {
			close(s.runDone)
			return
		}
		if s.clearAccess != nil {
			s.clearOnce.Do(s.clearAccess)
		}
		s.mu.Lock()
		s.runErr = err
		s.mu.Unlock()
		close(s.runDone)
	})
}

// ownsSDKLifecycle reports whether service holds the process-wide reservation.
func ownsSDKLifecycle(service *Service) bool {
	if service == nil || service.process == nil {
		return false
	}
	service.process.mu.Lock()
	defer service.process.mu.Unlock()
	return service.process.phase == sdkProcessConsumed &&
		service.process.lease == service.lease
}

// result returns the stable lifecycle result.
func (s *Service) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}
