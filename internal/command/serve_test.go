package command

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestServeFailsBeforeSDKConstruction catches fail-open startup when configuration, secrets,
// auth-directory preparation, or PostgreSQL readiness is unavailable.
func TestServeFailsBeforeSDKConstruction(t *testing.T) {
	tests := []struct {
		name   string
		change func(*serveDependencies, *config.Config)
	}{
		{name: "configuration", change: func(deps *serveDependencies, _ *config.Config) {
			deps.load = func(string, func(string) string) (config.Config, error) {
				return config.Config{}, errors.New("bad config")
			}
		}},
		{name: "pepper", change: func(_ *serveDependencies, cfg *config.Config) {
			cfg.LLMGW.KeyPepperEnv = "MISSING_PEPPER"
		}},
		{name: "auth directory", change: func(deps *serveDependencies, _ *config.Config) {
			deps.prepareAuthDir = func(string) error { return errors.New("unsafe auth directory") }
		}},
		{name: "postgres open", change: func(deps *serveDependencies, _ *config.Config) {
			deps.openStore = func(context.Context, string) (*serveStore, error) {
				return nil, errors.New("postgres unavailable")
			}
		}},
		{name: "postgres ping", change: func(deps *serveDependencies, _ *config.Config) {
			deps.openStore = func(context.Context, string) (*serveStore, error) {
				return &serveStore{
					ping:  func(context.Context) error { return errors.New("postgres unavailable") },
					close: func() {},
				}, nil
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validServeConfig()
			builds := 0
			deps := successfulServeDependencies(&cfg, nil)
			deps.buildService = func(config.Config, *serveStore, []byte) (serveService, error) {
				builds++
				return &fakeServeService{}, nil
			}
			test.change(&deps, &cfg)
			if err := runServeWith(context.Background(), nil, testRootStreams(serveEnvironment()), deps); err == nil {
				t.Fatal("serve succeeded")
			}
			if builds != 0 {
				t.Fatalf("SDK construction calls = %d, want 0", builds)
			}
		})
	}
}

// TestServeLifecycleOrder catches construction before crash recovery, workers starting before the
// SDK is built, or PostgreSQL closing before SDK usage and workers have drained.
func TestServeLifecycleOrder(t *testing.T) {
	var mu sync.Mutex
	var events []string
	add := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	cfg := validServeConfig()
	deps := successfulServeDependencies(&cfg, add)
	ctx, cancel := context.WithCancel(context.Background())
	service := &fakeServeService{run: func(runCtx context.Context) error {
		add("sdk-run")
		cancel()
		<-runCtx.Done()
		add("sdk-usage-drained")
		return nil
	}}
	deps.buildService = func(config.Config, *serveStore, []byte) (serveService, error) {
		add("sdk-construct")
		return service, nil
	}

	if err := runServeWith(ctx, nil, testRootStreams(serveEnvironment()), deps); err != nil {
		t.Fatalf("serve: %v", err)
	}
	want := []string{
		"load", "auth-dir", "open", "ping", "serve-lock-acquire", "recover", "sdk-construct",
		"workers-start", "sdk-run", "sdk-usage-drained", "workers-stop",
		"serve-lock-release", "postgres-close",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %#v, want %#v", events, want)
	}
}

// TestServeLockFailurePrecedesRecovery catches moving singleton acquisition
// after RecoverInterrupted, which would let a losing process mutate live rows.
func TestServeLockFailurePrecedesRecovery(t *testing.T) {
	cfg := validServeConfig()
	var recovered bool
	deps := successfulServeDependencies(&cfg, nil)
	lockErr := errors.New("serve lock held")
	deps.openStore = func(context.Context, string) (*serveStore, error) {
		return &serveStore{
			ping: func(context.Context) error { return nil },
			acquireServeLock: func(context.Context) (func(context.Context) error, error) {
				return nil, lockErr
			},
			recoverInterrupted: func(context.Context, time.Time) (int64, error) {
				recovered = true
				return 0, nil
			},
			close: func() {},
		}, nil
	}

	err := runServeWith(context.Background(), nil, testRootStreams(serveEnvironment()), deps)
	if !errors.Is(err, lockErr) {
		t.Fatalf("serve lock error = %v, want lock cause", err)
	}
	if recovered {
		t.Fatal("RecoverInterrupted ran after singleton acquisition failed")
	}
}

// TestServeJoinsLockReleaseError catches cleanup that drops either the original
// SDK failure or the dedicated-session unlock failure.
func TestServeJoinsLockReleaseError(t *testing.T) {
	cfg := validServeConfig()
	deps := successfulServeDependencies(&cfg, nil)
	runErr := errors.New("listener stopped")
	releaseErr := errors.New("unlock failed")
	deps.buildService = func(config.Config, *serveStore, []byte) (serveService, error) {
		return &fakeServeService{run: func(context.Context) error { return runErr }}, nil
	}
	store, err := deps.openStore(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	store.acquireServeLock = func(context.Context) (func(context.Context) error, error) {
		return func(context.Context) error { return releaseErr }, nil
	}
	deps.openStore = func(context.Context, string) (*serveStore, error) { return store, nil }

	err = runServeWith(context.Background(), nil, testRootStreams(serveEnvironment()), deps)
	if !errors.Is(err, runErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("serve cleanup error = %v, want joined run/unlock causes", err)
	}
}

// TestServePropagatesUnexpectedSDKReturn catches a composition root that treats an unexpected
// listener/service return as a clean shutdown and prevents the process manager from restarting it.
func TestServePropagatesUnexpectedSDKReturn(t *testing.T) {
	cfg := validServeConfig()
	deps := successfulServeDependencies(&cfg, nil)
	unexpected := errors.New("listener stopped")
	deps.buildService = func(config.Config, *serveStore, []byte) (serveService, error) {
		return &fakeServeService{run: func(context.Context) error { return unexpected }}, nil
	}
	err := runServeWith(context.Background(), nil, testRootStreams(serveEnvironment()), deps)
	if !errors.Is(err, unexpected) {
		t.Fatalf("serve error = %v, want listener error", err)
	}
}

// TestServeRejectsUnexpectedCleanSDKReturn catches a lifecycle mutation that lets an unrequested
// clean listener return exit the process successfully instead of asking its manager for a restart.
func TestServeRejectsUnexpectedCleanSDKReturn(t *testing.T) {
	cfg := validServeConfig()
	deps := successfulServeDependencies(&cfg, nil)
	deps.buildService = func(config.Config, *serveStore, []byte) (serveService, error) {
		return &fakeServeService{run: func(context.Context) error { return nil }}, nil
	}
	if err := runServeWith(
		context.Background(), nil, testRootStreams(serveEnvironment()), deps,
	); err == nil {
		t.Fatal("unexpected clean SDK return succeeded")
	}
}

func validServeConfig() config.Config {
	return config.Config{
		Proxy: &sdkconfig.Config{AuthDir: "/safe/auth"},
		LLMGW: config.LLMGW{
			PostgresDSNEnv:     "TEST_DSN",
			KeyPepperEnv:       "TEST_PEPPER",
			UsageRetentionDays: 35,
		},
		UsageRetention: 35 * 24 * time.Hour,
	}
}

func serveEnvironment() map[string]string {
	return map[string]string{
		"TEST_DSN":    "postgres://test",
		"TEST_PEPPER": "0123456789abcdef0123456789abcdef",
	}
}

func successfulServeDependencies(cfg *config.Config, add func(string)) serveDependencies {
	if add == nil {
		add = func(string) {}
	}
	store := &serveStore{
		ping: func(context.Context) error {
			add("ping")
			return nil
		},
		acquireServeLock: func(context.Context) (func(context.Context) error, error) {
			add("serve-lock-acquire")
			return func(context.Context) error {
				add("serve-lock-release")
				return nil
			}, nil
		},
		recoverInterrupted: func(context.Context, time.Time) (int64, error) {
			add("recover")
			return 0, nil
		},
		close: func() { add("postgres-close") },
	}
	return serveDependencies{
		load: func(string, func(string) string) (config.Config, error) {
			add("load")
			return *cfg, nil
		},
		prepareAuthDir: func(string) error {
			add("auth-dir")
			return nil
		},
		openStore: func(context.Context, string) (*serveStore, error) {
			add("open")
			return store, nil
		},
		buildService: func(config.Config, *serveStore, []byte) (serveService, error) {
			add("sdk-construct")
			return &fakeServeService{}, nil
		},
		startWorkers: func(ctx context.Context, _ *serveStore, _ time.Duration) <-chan struct{} {
			add("workers-start")
			done := make(chan struct{})
			go func() {
				<-ctx.Done()
				add("workers-stop")
				close(done)
			}()
			return done
		},
		now: time.Now,
	}
}

type fakeServeService struct {
	run func(context.Context) error
}

func (f *fakeServeService) Run(ctx context.Context) error {
	if f.run != nil {
		return f.run(ctx)
	}
	return nil
}

func (f *fakeServeService) Close() error { return nil }
