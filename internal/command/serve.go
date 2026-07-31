package command

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
)

const serveLockReleaseTimeout = 5 * time.Second

// serveService is the embedded SDK lifecycle owned by the composition root.
type serveService interface {
	Run(context.Context) error
	Close() error
}

// serveStore keeps lifecycle seams narrow while production retains the concrete PostgreSQL store.
type serveStore struct {
	postgres           *postgres.Store
	ping               func(context.Context) error
	acquireServeLock   func(context.Context) (func(context.Context) error, error)
	recoverInterrupted func(context.Context, time.Time) (int64, error)
	close              func()
}

type serveDependencies struct {
	load           func(string, func(string) string) (config.Config, error)
	prepareAuthDir func(string) error
	openStore      func(context.Context, string) (*serveStore, error)
	buildService   func(config.Config, *serveStore, []byte, *alert.Tracker) (serveService, error)
	startWorkers   func(context.Context, *serveStore, time.Duration, *alert.Tracker) <-chan struct{}
	now            func() time.Time
}

// runServe composes PostgreSQL governance around one embedded CLIProxyAPI service.
func runServe(ctx context.Context, args []string, streams Streams) error {
	return runServeWith(ctx, args, streams, productionServeDependencies())
}

func runServeWith(
	ctx context.Context,
	args []string,
	streams Streams,
	deps serveDependencies,
) (returnErr error) {
	streams = normalizedStreams(streams)
	if len(args) != 0 {
		return fmt.Errorf("serve accepts no arguments")
	}
	cfg, err := deps.load(configPath(streams), streams.Getenv)
	if err != nil {
		return fmt.Errorf("load serve configuration:\n%w", err)
	}
	pepper, err := cfg.KeyPepper(streams.Getenv)
	if err != nil {
		return fmt.Errorf("load serve key pepper:\n%w", err)
	}
	defer clear(pepper)
	if cfg.Proxy == nil {
		return errors.New("prepare serve auth directory:\nCLIProxyAPI configuration is required")
	}
	if err := deps.prepareAuthDir(cfg.Proxy.AuthDir); err != nil {
		return fmt.Errorf("prepare serve auth directory:\n%w", err)
	}
	tracker, webhook, err := newServeAlerting(ctx, cfg, streams)
	if err != nil {
		return err
	}
	serviceStarted := false
	// Registered before the lock-release defer, so LIFO runs it after: the last
	// thing the operator sees then matches the process actually ending.
	defer func() { stopAlerting(ctx, tracker, webhook, serviceStarted) }()
	dsn, err := cfg.DatabaseDSN(streams.Getenv)
	if err != nil {
		return fmt.Errorf("resolve serve database:\n%w", err)
	}
	store, err := deps.openStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open serve store:\n%w", err)
	}
	if store == nil || store.ping == nil || store.acquireServeLock == nil ||
		store.recoverInterrupted == nil || store.close == nil {
		if store != nil && store.close != nil {
			store.close()
		}
		return errors.New("open serve store:\nincomplete PostgreSQL lifecycle")
	}
	if err := store.ping(ctx); err != nil {
		store.close()
		return fmt.Errorf("ping serve store:\n%w", err)
	}
	releaseServeLock, err := store.acquireServeLock(ctx)
	if err != nil {
		store.close()
		return fmt.Errorf("acquire serve singleton lock:\n%w", err)
	}
	if releaseServeLock == nil {
		store.close()
		return errors.New("acquire serve singleton lock:\nincomplete PostgreSQL lock lifecycle")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(
			context.Background(),
			serveLockReleaseTimeout,
		)
		defer cancel()
		if err := releaseServeLock(releaseCtx); err != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("release serve singleton lock:\n%w", err),
			)
		}
		store.close()
	}()
	if _, err := store.recoverInterrupted(ctx, deps.now().UTC()); err != nil {
		return fmt.Errorf("recover interrupted requests:\n%w", err)
	}
	service, err := deps.buildService(cfg, store, pepper, tracker)
	if err != nil {
		return err
	}
	serviceStarted = true
	tracker.Emit(alert.KindGatewayStarted)

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	workersDone := deps.startWorkers(workerCtx, store, cfg.UsageRetention, tracker)
	runErr := service.Run(ctx)
	if runErr == nil && ctx.Err() == nil {
		runErr = errors.New("embedded CLIProxyAPI service returned unexpectedly")
	}
	cancelWorkers()
	<-workersDone
	return runErr
}

func productionServeDependencies() serveDependencies {
	return serveDependencies{
		load:           config.Load,
		prepareAuthDir: cliproxy.PrepareAuthDir,
		openStore: func(ctx context.Context, dsn string) (*serveStore, error) {
			store, err := postgres.New(ctx, dsn)
			if err != nil {
				return nil, err
			}
			return &serveStore{
				postgres: store,
				ping:     store.Ping,
				acquireServeLock: func(ctx context.Context) (func(context.Context) error, error) {
					lock, err := store.AcquireServeLock(ctx)
					if err != nil {
						return nil, err
					}
					return lock.Release, nil
				},
				recoverInterrupted: store.RecoverInterrupted,
				close:              store.Close,
			}, nil
		},
		buildService: buildServeService,
		startWorkers: func(
			ctx context.Context,
			store *serveStore,
			retention time.Duration,
			tracker *alert.Tracker,
		) <-chan struct{} {
			return StartWorkers(ctx, store.postgres, retention, tracker)
		},
		now: time.Now,
	}
}

func buildServeService(
	cfg config.Config,
	store *serveStore,
	pepper []byte,
	tracker *alert.Tracker,
) (serveService, error) {
	if store == nil || store.postgres == nil {
		return nil, errors.New("construct serve service:\nPostgreSQL store is required")
	}
	keys, err := projectkey.NewService(store.postgres, pepper, rand.Reader, time.Now)
	if err != nil {
		return nil, fmt.Errorf("construct project-key service:\n%w", err)
	}
	bridge, err := cliproxy.NewUsageBridge(rand.Reader, cfg.LLMGW.UsageOutstandingCapacity)
	if err != nil {
		return nil, fmt.Errorf("construct usage bridge:\n%w", err)
	}
	middleware := cliproxy.NewMiddleware(keys, store.postgres, time.Now, bridge, tracker)
	usage := cliproxy.NewUsagePlugin(
		store.postgres,
		bridge,
		postgres.IsTransientUsageError,
	)
	service, err := cliproxy.NewService(cfg, middleware, usage, cliproxy.NewAlertUsagePlugin(tracker))
	if err != nil {
		return nil, err
	}
	// The poisoned state is terminal and would otherwise leave a live process
	// answering 503 to every generation. Stop instead, so the supervisor can
	// restart a healthy one.
	bridge.ReportPoisonWith(func() {
		log.Print("llmgw: usage correlation is unrecoverable, stopping the service")
		tracker.Emit(alert.KindUsagePoisoned)
		go func() { _ = service.Close() }()
	})
	return service, nil
}
