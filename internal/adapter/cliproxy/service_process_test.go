package cliproxy

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	log "github.com/sirupsen/logrus"
)

func TestSDKProcessStateReleasesOnlyUnstartedConstruction(t *testing.T) {
	state := newSDKProcessState()
	first, err := state.reserveConstruction()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleActive) {
		t.Fatalf("second construction error = %v, want active", err)
	}

	state.releaseConstruction(first)
	second, err := state.reserveConstruction()
	if err != nil {
		t.Fatalf("construction after Close-before-Run = %v", err)
	}
	state.releaseConstruction(second)
}

func TestSDKProcessStateConsumptionIsPermanent(t *testing.T) {
	state := newSDKProcessState()
	lease, err := state.reserveConstruction()
	if err != nil {
		t.Fatal(err)
	}
	if err := state.consume(lease); err != nil {
		t.Fatal(err)
	}
	state.releaseConstruction(lease)

	if _, err := state.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleConsumed) {
		t.Fatalf("construction after consumed Run = %v, want consumed", err)
	}
}

func TestServiceRegistersUsageOnlyForFirstRun(t *testing.T) {
	t.Run("Close before Run", func(t *testing.T) {
		service := newLifecycleService(
			newBlockingFakeLifecycle(),
			closedSignal(),
			nil,
			time.Second,
		)
		var registrations atomic.Int64
		service.registerUsage = func() { registrations.Add(1) }

		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if got := registrations.Load(); got != 0 {
			t.Fatalf("usage registrations = %d, want 0", got)
		}
	})

	t.Run("first Run", func(t *testing.T) {
		proxy := newBlockingFakeLifecycle()
		service := newLifecycleService(proxy, closedSignal(), nil, time.Second)
		var registrations atomic.Int64
		service.registerUsage = func() { registrations.Add(1) }
		done := make(chan error, 1)
		go func() { done <- service.Run(context.Background()) }()
		<-proxy.runEntered

		if err := service.Run(context.Background()); !errors.Is(err, ErrServiceAlreadyRun) {
			t.Fatalf("second Run error = %v, want one-shot", err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := registrations.Load(); got != 1 {
			t.Fatalf("usage registrations = %d, want 1", got)
		}
	})
}

func TestServiceCloseKeepsConstructionLeaseThroughGlobalCleanup(t *testing.T) {
	process := newSDKProcessState()
	lease, err := process.reserveConstruction()
	if err != nil {
		t.Fatal(err)
	}
	clearEntered := make(chan struct{})
	allowClear := make(chan struct{})
	service := newLifecycleServiceWithLease(
		newBlockingFakeLifecycle(),
		closedSignal(),
		func() {
			close(clearEntered)
			<-allowClear
		},
		time.Second,
		process,
		lease,
	)
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- service.Close()
	}()
	<-clearEntered

	if _, err := process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleActive) {
		t.Fatalf("construction during old cleanup = %v, want active", err)
	}
	close(allowClear)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	next, err := process.reserveConstruction()
	if err != nil {
		t.Fatalf("construction after complete cleanup = %v", err)
	}
	process.releaseConstruction(next)
}

func TestNewServiceBuildFailureKeepsLeaseThroughSnapshotCleanup(t *testing.T) {
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	process := newSDKProcessState()
	cleanupEntered := make(chan struct{})
	allowCleanup := make(chan struct{})
	var cleanupCalls atomic.Int64
	hooks := serviceConstructionHooks{
		process: process,
		snapshot: func(cfg config.Config) (*sdkStartupSnapshot, error) {
			snapshot, err := newSDKStartupSnapshot(cfg)
			if err != nil {
				return nil, err
			}
			snapshot.removeAuthDir = func(string) error {
				cleanupCalls.Add(1)
				close(cleanupEntered)
				<-allowCleanup
				return nil
			}
			return snapshot, nil
		},
		build: func() (constructedSDK, func(), error) {
			return nil, nil, errors.New("forced SDK Build failure")
		},
	}
	bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
	done := make(chan error, 1)
	go func() {
		_, err := newService(
			cfg,
			NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge),
			NewUsagePlugin(
				successfulUsageRepository(func(governance.UsageAttempt) {}),
				bridge,
				nil,
			),
			hooks,
		)
		done <- err
	}()
	select {
	case <-cleanupEntered:
	case err := <-done:
		t.Fatalf("NewService returned before snapshot cleanup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("snapshot cleanup was never entered")
	}

	if _, err := process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleActive) {
		t.Fatalf("construction during failed Build cleanup = %v, want active", err)
	}
	close(allowCleanup)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "forced SDK Build failure") {
		t.Fatalf("NewService Build failure = %v, want forced cause", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("snapshot cleanup calls = %d, want 1", got)
	}
	next, err := process.reserveConstruction()
	if err != nil {
		t.Fatalf("construction after successful failure cleanup = %v", err)
	}
	process.releaseConstruction(next)
}

func TestNewServiceBuildFailureCleanupErrorRetainsLease(t *testing.T) {
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	process := newSDKProcessState()
	cleanupErr := errors.New("forced snapshot cleanup failure")
	hooks := serviceConstructionHooks{
		process: process,
		snapshot: func(cfg config.Config) (*sdkStartupSnapshot, error) {
			snapshot, err := newSDKStartupSnapshot(cfg)
			if err != nil {
				return nil, err
			}
			snapshot.removeAuthDir = func(string) error { return cleanupErr }
			return snapshot, nil
		},
		build: func() (constructedSDK, func(), error) {
			return nil, nil, errors.New("forced SDK Build failure")
		},
	}
	bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
	service, err := newService(
		cfg,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge),
		NewUsagePlugin(
			successfulUsageRepository(func(governance.UsageAttempt) {}),
			bridge,
			nil,
		),
		hooks,
	)

	if service != nil || err == nil ||
		!strings.Contains(err.Error(), "remove SDK startup auth snapshot: unavailable") {
		t.Fatalf("NewService cleanup failure = (%#v, %v), want nil/safe cleanup cause",
			service, err)
	}
	if _, err := process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleActive) {
		t.Fatalf("construction after failed Build cleanup = %v, want active", err)
	}
}

func TestServiceStartupFailureAfterRegistrationConsumesProcess(t *testing.T) {
	previousLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	defer log.SetLevel(previousLevel)

	service := newLifecycleService(
		newBlockingFakeLifecycle(),
		closedSignal(),
		nil,
		time.Second,
	)
	var registrations atomic.Int64
	service.registerUsage = func() { registrations.Add(1) }

	err := service.Run(context.Background())
	if !errors.Is(err, ErrSDKStartupCompatibility) {
		t.Fatalf("Run error = %v, want startup compatibility", err)
	}
	if got := registrations.Load(); got != 1 {
		t.Fatalf("usage registrations = %d, want 1", got)
	}
	if _, err := service.process.reserveConstruction(); !errors.Is(err, ErrSDKLifecycleConsumed) {
		t.Fatalf("construction after failed startup = %v, want consumed", err)
	}
}
