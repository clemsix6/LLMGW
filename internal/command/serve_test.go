package command

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

// testRootStreams builds command streams reading a fixed environment.
func testRootStreams(environment map[string]string) Streams {
	return Streams{
		In:  strings.NewReader(""),
		Out: new(bytes.Buffer),
		Err: new(bytes.Buffer),
		Getenv: func(name string) string {
			return environment[name]
		},
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
