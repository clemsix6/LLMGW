package cliproxy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

// TestNewServiceBuildsSecureSDK verifies construction through the Task 5 security boundary.
func TestNewServiceBuildsSecureSDK(t *testing.T) {
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge)
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {}),
		bridge,
		nil,
	)

	service, err := NewService(
		cfg,
		middleware,
		plugin,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close service: %v", err)
		}
	})

	if service.proxy == nil {
		t.Fatal("proxy is nil")
	}
	providers := sdkaccess.RegisteredProviders()
	if len(providers) != 1 || providers[0].Identifier() != AccessProviderType {
		t.Fatalf("registered access providers = %v, want only LLMGW", providerIdentifiers(providers))
	}
}

func TestNewServiceOwnsFrozenStartupSnapshotAndCleansBeforeRun(t *testing.T) {
	fixture := newSDKFixture(t)
	sourceAuth := filepath.Join(fixture.authDir, "credential.json")
	if err := os.WriteFile(sourceAuth, []byte(`{"type":"codex","access_token":"initial"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := boundedServiceConfig(fixture)
	cfg.Proxy.OAuthExcludedModels = map[string][]string{"codex": {"model-a"}}
	bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
	service, err := NewService(
		cfg,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge),
		NewUsagePlugin(successfulUsageRepository(func(governance.UsageAttempt) {}), bridge, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if service.startup == nil ||
		service.startup.config.Proxy == cfg.Proxy ||
		service.startup.config.Proxy.AuthDir == fixture.authDir {
		t.Fatal("service did not retain a private frozen startup snapshot")
	}
	snapshotDir := service.startup.config.Proxy.AuthDir
	cfg.Proxy.RequestRetry = 200
	cfg.Proxy.OAuthExcludedModels["codex"][0] = "mutated"
	if err := os.WriteFile(sourceAuth, []byte(`{"request_retry":200}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if service.startup.config.Proxy.RequestRetry != 0 ||
		service.startup.config.Proxy.OAuthExcludedModels["codex"][0] != "model-a" {
		t.Fatal("service startup configuration changed through caller pointer")
	}
	if err := service.startup.config.ValidateUsageBackpressure(); err != nil {
		t.Fatalf("frozen service bound became invalid: %v", err)
	}

	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("Close-before-Run retained auth snapshot: %v", err)
	}
}

func TestNewServiceReservesProcessBeforeSecondSDKBuild(t *testing.T) {
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	newComponents := func() (*Middleware, *UsagePlugin) {
		bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
		return NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge),
			NewUsagePlugin(
				successfulUsageRepository(func(governance.UsageAttempt) {}),
				bridge,
				nil,
			)
	}
	firstMiddleware, firstPlugin := newComponents()
	first, err := NewService(cfg, firstMiddleware, firstPlugin)
	if err != nil {
		t.Fatal(err)
	}

	secondMiddleware, secondPlugin := newComponents()
	second, err := NewService(cfg, secondMiddleware, secondPlugin)
	if second != nil || !errors.Is(err, ErrSDKLifecycleActive) {
		t.Fatalf("second construction = (%#v, %v), want nil/active", second, err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	thirdMiddleware, thirdPlugin := newComponents()
	third, err := NewService(cfg, thirdMiddleware, thirdPlugin)
	if err != nil {
		t.Fatalf("construction after Close-before-Run: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestNewServiceReturnsContextualBuildErrorAndCleansAccess verifies failure cleanup.
func TestNewServiceReturnsContextualBuildErrorAndCleansAccess(t *testing.T) {
	bridge := fixedUsageBridge(t)
	service, err := NewService(
		config.Config{},
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge),
		NewUsagePlugin(successfulUsageRepository(func(governance.UsageAttempt) {}), bridge, nil),
	)
	if service != nil {
		t.Fatal("service is non-nil")
	}
	if err == nil || !strings.Contains(err.Error(), "construct embedded CLIProxyAPI service") {
		t.Fatalf("NewService error = %v", err)
	}
	for _, provider := range sdkaccess.RegisteredProviders() {
		if provider.Identifier() == AccessProviderType {
			t.Fatal("LLMGW access provider remains registered after build failure")
		}
	}
}

func TestNewServiceRequiresBridgeBoundComponents(t *testing.T) {
	for _, test := range []struct {
		name       string
		middleware *Middleware
		plugin     *UsagePlugin
		want       string
	}{
		{name: "middleware", want: "bridge-bound middleware is required"},
		{
			name:       "plugin",
			middleware: NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, fixedUsageBridge(t)),
			want:       "bridge-bound usage plugin is required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewService(config.Config{}, test.middleware, test.plugin)
			if service != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewService components = (%#v, %v), want %q", service, err, test.want)
			}
		})
	}
}

func TestNewServiceRejectsUsageBridgeMismatch(t *testing.T) {
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	middlewareBridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
	pluginBridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)

	service, err := NewService(
		cfg,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, middlewareBridge),
		NewUsagePlugin(
			successfulUsageRepository(func(governance.UsageAttempt) {}),
			pluginBridge,
			nil,
		),
	)
	if service != nil || err == nil || !strings.Contains(err.Error(), "usage bridge mismatch") {
		t.Fatalf("NewService bridge mismatch = (%#v, %v), want nil, error", service, err)
	}
}

func TestNewServiceRejectsUsageCapacityMismatchAndDuplicatePlugin(t *testing.T) {
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)
	middleware := NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge)
	plugin := NewUsagePlugin(
		successfulUsageRepository(func(governance.UsageAttempt) {}),
		bridge,
		nil,
	)

	mismatched := cfg
	mismatched.LLMGW.UsageOutstandingCapacity--
	if service, err := NewService(mismatched, middleware, plugin); service != nil ||
		err == nil || !strings.Contains(err.Error(), "usage bridge capacity mismatch") {
		t.Fatalf("capacity mismatch = (%#v, %v), want nil, error", service, err)
	}
	if service, err := NewService(cfg, middleware, plugin, plugin); service != nil ||
		err == nil || !strings.Contains(err.Error(), "only one LLMGW usage plugin") {
		t.Fatalf("duplicate plugin = (%#v, %v), want nil, error", service, err)
	}
}

func TestNewServiceRejectsHostileStartupAuthRetryFile(t *testing.T) {
	fixture := newSDKFixture(t)
	if err := os.WriteFile(
		filepath.Join(fixture.authDir, "hostile.json"),
		[]byte(`{"provider":"codex","metadata":{"request_retry":1}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	cfg := boundedServiceConfig(fixture)
	bridge := fixedUsageBridgeCapacity(t, cfg.LLMGW.UsageOutstandingCapacity)

	service, err := NewService(
		cfg,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, bridge),
		NewUsagePlugin(
			successfulUsageRepository(func(governance.UsageAttempt) {}),
			bridge,
			nil,
		),
	)
	if service != nil || err == nil ||
		!strings.Contains(err.Error(), "auth retry override exceeds request-retry") {
		t.Fatalf("NewService hostile auth file = (%#v, %v), want nil, rejection",
			service, err)
	}
}

func boundedServiceConfig(fixture sdkFixture) config.Config {
	fixture.config.RequestRetry = 0
	fixture.config.MaxRetryCredentials = 1
	fixture.config.Routing.SessionAffinity = false
	return config.Config{
		Path:            fixture.configPath,
		Proxy:           fixture.config,
		LLMGW:           config.LLMGW{UsageOutstandingCapacity: 64},
		MaxUsageRecords: 2,
	}
}

// providerIdentifiers converts providers to safe test diagnostics.
func providerIdentifiers(providers []sdkaccess.Provider) []string {
	identifiers := make([]string, 0, len(providers))
	for _, provider := range providers {
		identifiers = append(identifiers, provider.Identifier())
	}
	return identifiers
}
