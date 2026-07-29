package cliproxy

import (
	"errors"

	"github.com/clemsix6/LLMGW/internal/config"
)

// sdkStartupSnapshot owns the only SDK-visible configuration.
type sdkStartupSnapshot struct {
	config config.Config
}

// newSDKStartupSnapshot freezes the configuration the SDK is built with.
//
// Only configuration is frozen. The auth directory stays the operator's own:
// the SDK writes there for the whole run, rotating provider credentials and
// recording cooldown state beside them. Handing it a private copy would strand
// every refreshed token in a directory deleted at shutdown, so the next start
// would replay a refresh token the provider already consumed — a total
// authentication failure with the providers that rotate. Validation therefore
// reads the same tree the SDK will.
func newSDKStartupSnapshot(cfg config.Config) (*sdkStartupSnapshot, error) {
	if cfg.Proxy == nil {
		return nil, errors.New("snapshot SDK startup configuration: configuration is required")
	}
	frozen := cfg
	frozen.Proxy = cfg.Proxy.CloneForRuntime()
	if frozen.Proxy == nil {
		return nil, errors.New("snapshot SDK startup configuration: clone failed")
	}
	snapshot := &sdkStartupSnapshot{config: frozen}
	if err := snapshot.config.ValidateUsageBackpressure(); err != nil {
		return nil, err
	}
	return snapshot, nil
}
