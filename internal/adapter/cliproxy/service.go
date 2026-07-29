package cliproxy

import (
	"context"
	"errors"
	"fmt"

	"github.com/clemsix6/LLMGW/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	sdkproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	sdkusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdklogging "github.com/router-for-me/CLIProxyAPI/v7/sdk/logging"
)

// NewService builds one secured, one-shot embedded CLIProxyAPI service.
//
// Call Run exactly once. Call Close when construction succeeds but Run will not
// be called; Close is also safe during or after Run.
func NewService(
	cfg config.Config,
	middleware *Middleware,
	usagePlugin *UsagePlugin,
	plugins ...sdkusage.Plugin,
) (*Service, error) {
	return newService(
		cfg,
		middleware,
		usagePlugin,
		serviceConstructionHooks{},
		plugins...,
	)
}

// constructedSDK is the SDK surface required after a successful Build.
type constructedSDK interface {
	proxyLifecycle

	// RegisterUsagePlugin appends one SDK usage observer.
	RegisterUsagePlugin(sdkusage.Plugin)
}

// serviceConstructionHooks supplies isolated failure seams to lifecycle tests.
type serviceConstructionHooks struct {
	process  *sdkProcessState                                 // process overrides the package-global lease owner.
	snapshot func(config.Config) (*sdkStartupSnapshot, error) // snapshot freezes SDK inputs.
	build    func() (constructedSDK, func(), error)           // build constructs the secured SDK.
}

// newService builds one service with optional private construction seams.
func newService(
	cfg config.Config,
	middleware *Middleware,
	usagePlugin *UsagePlugin,
	hooks serviceConstructionHooks,
	plugins ...sdkusage.Plugin,
) (*Service, error) {
	if err := validateServiceComposition(cfg, middleware, usagePlugin, plugins); err != nil {
		return nil, err
	}
	process := constructionProcess(hooks)
	lease, err := process.reserveConstruction()
	if err != nil {
		return nil, fmt.Errorf("construct embedded CLIProxyAPI service:\n%w", err)
	}
	startup, err := constructionSnapshot(hooks)(cfg)
	if err != nil {
		process.releaseConstruction(lease)
		return nil, fmt.Errorf("construct embedded CLIProxyAPI service:\n%w", err)
	}

	started := make(chan struct{})
	var service *Service
	build := serviceBuild(hooks, startup, middleware, &service, started)
	proxy, clearAccess, err := build()
	if err != nil {
		return failServiceBuild(process, lease, startup, err)
	}
	service = assembleService(
		proxy, clearAccess, started, process, lease, startup,
		middleware.bridge, usagePlugin, plugins,
	)
	return service, nil
}

// validateServiceComposition rejects inconsistent bridge and plugin wiring.
func validateServiceComposition(
	cfg config.Config,
	middleware *Middleware,
	usagePlugin *UsagePlugin,
	plugins []sdkusage.Plugin,
) error {
	if middleware == nil || middleware.bridge == nil {
		return fmt.Errorf("construct embedded CLIProxyAPI service:\nbridge-bound middleware is required")
	}
	if usagePlugin == nil || usagePlugin.bridge == nil {
		return fmt.Errorf("construct embedded CLIProxyAPI service:\nbridge-bound usage plugin is required")
	}
	if middleware.bridge != usagePlugin.bridge {
		return fmt.Errorf("construct embedded CLIProxyAPI service:\nusage bridge mismatch")
	}
	if cfg.LLMGW.UsageOutstandingCapacity != middleware.bridge.capacity {
		return fmt.Errorf("construct embedded CLIProxyAPI service:\nusage bridge capacity mismatch")
	}
	for _, plugin := range plugins {
		if _, duplicate := plugin.(*UsagePlugin); duplicate {
			return fmt.Errorf(
				"construct embedded CLIProxyAPI service:\nonly one LLMGW usage plugin is allowed",
			)
		}
	}
	return nil
}

// constructionProcess selects the production or isolated lease owner.
func constructionProcess(hooks serviceConstructionHooks) *sdkProcessState {
	if hooks.process != nil {
		return hooks.process
	}
	return defaultSDKProcessState
}

// constructionSnapshot selects the production or isolated snapshotter.
func constructionSnapshot(
	hooks serviceConstructionHooks,
) func(config.Config) (*sdkStartupSnapshot, error) {
	if hooks.snapshot != nil {
		return hooks.snapshot
	}
	return newSDKStartupSnapshot
}

// serviceBuild selects the production secured SDK build or a failure seam.
func serviceBuild(
	hooks serviceConstructionHooks,
	startup *sdkStartupSnapshot,
	middleware *Middleware,
	owner **Service,
	started chan struct{},
) func() (constructedSDK, func(), error) {
	if hooks.build != nil {
		return hooks.build
	}
	frozen := startup.config
	builder := sdkproxy.NewBuilder().
		WithConfigPath(frozen.Path).
		WithWatcherFactory(startupOnlyWatcherFactory).
		WithServerOptions(sdkapi.WithRequestLoggerFactory(nilRequestLogger))
	return func() (constructedSDK, func(), error) {
		return buildSecureSDKWithAfterStart(
			builder,
			frozen.Proxy,
			sdkaccess.NewManager(),
			middleware.Handler(),
			AccessProvider{bridge: middleware.bridge},
			func(*sdkproxy.Service) { armSDKStartupBarrier(*owner, started) },
		)
	}
}

// failServiceBuild cleans the snapshot before releasing construction.
func failServiceBuild(
	process *sdkProcessState,
	lease *sdkConstructionLease,
	startup *sdkStartupSnapshot,
	buildErr error,
) (*Service, error) {
	cleanupErr := startup.Cleanup()
	if cleanupErr == nil {
		process.releaseConstruction(lease)
	}
	return nil, fmt.Errorf(
		"construct embedded CLIProxyAPI service:\n%w",
		errors.Join(buildErr, cleanupErr),
	)
}

// assembleService attaches lifecycle and usage ownership after a successful Build.
func assembleService(
	proxy constructedSDK,
	clearAccess func(),
	started <-chan struct{},
	process *sdkProcessState,
	lease *sdkConstructionLease,
	startup *sdkStartupSnapshot,
	bridge *UsageBridge,
	usagePlugin *UsagePlugin,
	plugins []sdkusage.Plugin,
) *Service {
	service := newLifecycleServiceWithLease(
		proxy,
		started,
		clearAccess,
		serviceShutdownTimeout,
		process,
		lease,
	)
	service.usageBridge = bridge
	service.startup = startup
	extraPlugins := append([]sdkusage.Plugin(nil), plugins...)
	service.registerUsage = func() {
		proxy.RegisterUsagePlugin(usagePlugin)
		for _, plugin := range extraPlugins {
			if plugin != nil {
				proxy.RegisterUsagePlugin(nonBarrierUsagePlugin{
					bridge: bridge,
					next:   plugin,
				})
			}
		}
	}
	return service
}

// startupOnlyWatcherFactory disables runtime config/auth mutation. Auth files
// and the retry/model bound are validated before Build; changes require restart.
func startupOnlyWatcherFactory(
	string,
	string,
	func(*sdkconfig.Config),
) (*sdkproxy.WatcherWrapper, error) {
	return nil, nil
}

// nonBarrierUsagePlugin keeps the LLMGW control record private from optional
// observers while preserving their normal SDK usage stream.
type nonBarrierUsagePlugin struct {
	bridge *UsageBridge
	next   sdkusage.Plugin
}

func (p nonBarrierUsagePlugin) HandleUsage(ctx context.Context, record sdkusage.Record) {
	if _, ok := p.bridge.barrierRequestID(record.APIKey); ok {
		return
	}
	if _, ok := p.bridge.cancelRequestID(record.APIKey); ok {
		return
	}
	p.next.HandleUsage(ctx, record)
}

// nilRequestLogger disables SDK request and error body capture.
func nilRequestLogger(*sdkconfig.Config, string) sdklogging.RequestLogger {
	return nil
}
