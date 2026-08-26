package cliproxy

import (
	"errors"
	"fmt"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	sdkproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// buildSecureSDKWithAfterStart builds the secure SDK and exposes established startup internally.
func buildSecureSDKWithAfterStart(
	builder *sdkproxy.Builder,
	cfg *sdkconfig.Config,
	manager *sdkaccess.Manager,
	middleware gin.HandlerFunc,
	provider sdkaccess.Provider,
	afterStart func(*sdkproxy.Service),
) (*sdkproxy.Service, func(), error) {
	if err := validateSecureSDKInputs(builder, cfg, manager, middleware, provider); err != nil {
		return nil, nil, err
	}

	applyCooldownConfig(cfg)
	armUpstreamFailureRedaction()
	clear := RegisterExclusiveAccess(provider)
	enforceExclusiveAccess(manager, provider)
	builder.WithConfig(cfg).
		WithRequestAccessManager(manager).
		WithWatcherFactory(disabledWatcherFactory).
		WithHooks(exclusiveAccessHooks(manager, provider, afterStart)).
		WithServerOptions(secureServerOption(middleware))

	service, err := builder.Build()
	if err != nil {
		clear()
		return nil, nil, fmt.Errorf("build secure embedded proxy:\n%w", err)
	}
	enforceExclusiveAccess(manager, provider)
	return service, clear, nil
}

// applyCooldownConfig publishes the cooldown settings the SDK reads from
// process globals rather than from the config it is built with. Only the
// upstream CLIProxyAPI binary sets them, so an embedding host that skips this
// silently runs the legacy one-minute transient cooldown and ignores both
// disable-cooling and transient-error-cooldown-seconds.
func applyCooldownConfig(cfg *sdkconfig.Config) {
	sdkauth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	sdkauth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
}

// validateSecureSDKInputs rejects runtime modes that can replace LLMGW access policy.
func validateSecureSDKInputs(
	builder *sdkproxy.Builder,
	cfg *sdkconfig.Config,
	manager *sdkaccess.Manager,
	middleware gin.HandlerFunc,
	provider sdkaccess.Provider,
) error {
	if builder == nil || cfg == nil || manager == nil || middleware == nil || provider == nil {
		return errors.New("secure embedded proxy dependencies are required")
	}
	if cfg.Home.Enabled {
		return errors.New("Home control plane is incompatible with LLMGW access policy")
	}
	if cfg.Plugins.Enabled {
		return errors.New("dynamic plugins are incompatible with LLMGW access policy")
	}
	return nil
}

// secureServerOption installs governance before native logging and disables redirect bypasses.
func secureServerOption(middleware gin.HandlerFunc) sdkapi.ServerOption {
	return sdkapi.WithEngineConfigurator(func(engine *gin.Engine) {
		engine.RedirectTrailingSlash = false
		engine.RedirectFixedPath = false
		engine.Use(middleware)
	})
}

// exclusiveAccessHooks restores LLMGW access after the SDK's startup reconciliation.
func exclusiveAccessHooks(
	manager *sdkaccess.Manager,
	provider sdkaccess.Provider,
	afterStart func(*sdkproxy.Service),
) sdkproxy.Hooks {
	return sdkproxy.Hooks{
		OnBeforeStart: func(*sdkconfig.Config) {
			enforceExclusiveAccess(manager, provider)
		},
		OnAfterStart: afterStart,
	}
}

// enforceExclusiveAccess restricts both the global registry and concrete server manager.
func enforceExclusiveAccess(manager *sdkaccess.Manager, provider sdkaccess.Provider) {
	sdkaccess.RegisterProvider(AccessProviderType, provider)
	sdkaccess.SetExclusiveProvider(AccessProviderType)
	manager.SetProviders([]sdkaccess.Provider{provider})
}

// disabledWatcherFactory prevents every SDK file reconciliation from replacing access policy.
func disabledWatcherFactory(
	string,
	string,
	func(*sdkconfig.Config),
) (*sdkproxy.WatcherWrapper, error) {
	return &sdkproxy.WatcherWrapper{}, nil
}
