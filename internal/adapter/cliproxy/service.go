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
	return newService(cfg, middleware, usagePlugin, plugins...)
}

// constructedSDK is the SDK surface required after a successful Build.
type constructedSDK interface {
	proxyLifecycle

	// RegisterUsagePlugin appends one SDK usage observer.
	RegisterUsagePlugin(sdkusage.Plugin)
}

// newService builds one service bound to the embedded SDK.
func newService(
	cfg config.Config,
	middleware *Middleware,
	usagePlugin *UsagePlugin,
	plugins ...sdkusage.Plugin,
) (*Service, error) {
	if err := validateServiceComposition(cfg, middleware, usagePlugin, plugins); err != nil {
		return nil, err
	}
	process := defaultSDKProcessState
	lease, err := process.reserveConstruction()
	if err != nil {
		return nil, fmt.Errorf("construct embedded CLIProxyAPI service:\n%w", err)
	}
	startup, err := newSDKStartupSnapshot(cfg)
	if err != nil {
		process.releaseConstruction(lease)
		return nil, fmt.Errorf("construct embedded CLIProxyAPI service:\n%w", err)
	}

	started := make(chan struct{})
	middleware.serveWhenReady(started)
	var service *Service
	build := serviceBuild(startup, middleware, &service, started)
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

// serviceBuild builds the secured SDK bound to this service.
func serviceBuild(
	startup *sdkStartupSnapshot,
	middleware *Middleware,
	owner **Service,
	started chan struct{},
) func() (constructedSDK, func(), error) {
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
