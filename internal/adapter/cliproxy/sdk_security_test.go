package cliproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	callbackCode  = "oauth-secret-code"
	callbackState = "oauth-secret-state"
)

func TestSecureSDKRejectsDynamicPlugins(t *testing.T) {
	fixture := newSDKFixture(t)
	fixture.config.Plugins.Enabled = true
	fixture.config.Plugins.Dir = t.TempDir()

	service, _, clear, err := buildGovernedSDK(
		fixture.config,
		fixture.configPath,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, fixedUsageBridge(t)).Handler(),
		false,
	)
	if clear != nil {
		defer clear()
	}
	if service != nil {
		t.Cleanup(func() {
			_ = service.Shutdown(context.Background())
		})
	}
	if err == nil {
		t.Fatal("dynamic plugin configuration was accepted")
	}
}

func TestSecureSDKRejectsHomeBeforeBuildSideEffects(t *testing.T) {
	fixture := newSDKFixture(t)
	fixture.config.Home.Enabled = true
	fixture.config.Home.Host = "home-control-plane-secret.example"

	rogue := &rogueAccessProvider{}
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{rogue})
	builder := sdkproxy.NewBuilder().
		WithConfig(fixture.config).
		WithConfigPath(fixture.configPath)

	service, clear, err := BuildSecureSDK(
		builder,
		fixture.config,
		manager,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, fixedUsageBridge(t)).Handler(),
		AccessProvider{},
	)
	if clear != nil {
		defer clear()
	}
	if service != nil {
		t.Cleanup(func() {
			_ = service.Shutdown(context.Background())
		})
	}
	if err == nil {
		t.Error("Home control plane configuration was accepted")
	} else if strings.Contains(err.Error(), fixture.config.Home.Host) {
		t.Errorf("Home validation error leaked configuration: %v", err)
	}

	providers := manager.Providers()
	if len(providers) != 1 || providers[0] != rogue {
		t.Errorf("access manager changed before Home validation: %v", providers)
	}
}

func TestSecureSDKPreservesExclusiveAccessAfterBuild(t *testing.T) {
	fixture := newSDKFixture(t)
	service, manager, clear, err := buildGovernedSDK(
		fixture.config,
		fixture.configPath,
		NewMiddleware(&fakeKeys{}, &fakeRequests{}, time.Now, fixedUsageBridge(t)).Handler(),
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer clear()
	if service == nil {
		t.Fatal("service is nil")
	}
	assertOnlyLLMGWProvider(t, manager)
}

func TestSecureSDKDeniedRoutesPrecedeLoggerRedirectAndReload(t *testing.T) {
	fixture := newSDKFixture(t)
	keys := &fakeKeys{}
	requests := &fakeRequests{}
	managerLogs := &lockedBuffer{}
	restoreLogs := captureLogrus(t, managerLogs)
	defer restoreLogs()

	service, manager, clear, err := buildGovernedSDK(
		fixture.config,
		fixture.configPath,
		NewMiddleware(keys, requests, time.Now, fixedUsageBridge(t)).Handler(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer clear()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.Run(ctx)
	}()
	waitForSDK(t, fixture.baseURL)
	assertOnlyLLMGWProvider(t, manager)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	exact := callbackRequest(t, client, fixture.baseURL+"/anthropic/callback")
	if exact.StatusCode != http.StatusNotFound {
		t.Errorf("exact callback status = %d, want 404", exact.StatusCode)
	}
	_ = exact.Body.Close()

	trailing := callbackRequest(t, client, fixture.baseURL+"/anthropic/callback/")
	if trailing.StatusCode != http.StatusNotFound {
		t.Errorf("trailing callback status = %d, want 404", trailing.StatusCode)
	}
	if location := trailing.Header.Get("Location"); location != "" {
		t.Errorf("trailing callback location = %q, want empty", location)
	}
	_ = trailing.Body.Close()

	logged := managerLogs.String()
	for _, secret := range []string{callbackCode, callbackState} {
		if strings.Contains(logged, secret) {
			t.Errorf("denied callback logs leaked %q:\n%s", secret, logged)
		}
	}
	if keys.calls != 0 || requests.calls() != 0 {
		t.Errorf("denied callback dependency calls = auth:%d repository:%d", keys.calls, requests.calls())
	}

	waitForLog(t, managerLogs, "file watcher started")
	rewriteSDKConfig(t, fixture, true)
	assertExclusiveDuringReloadWindow(t, manager)

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("service Run error = %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop")
	}
}

func buildGovernedSDK(
	cfg *sdkconfig.Config,
	configPath string,
	middleware gin.HandlerFunc,
	registerRogue bool,
) (*sdkproxy.Service, *sdkaccess.Manager, func(), error) {
	rogue := &rogueAccessProvider{}
	if registerRogue {
		sdkaccess.RegisterProvider(rogue.Identifier(), rogue)
	}

	manager := sdkaccess.NewManager()
	builder := sdkproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath)
	service, clearLLMGW, err := BuildSecureSDK(builder, cfg, manager, middleware, AccessProvider{})
	clear := func() {
		if clearLLMGW != nil {
			clearLLMGW()
		}
		sdkaccess.UnregisterProvider(rogue.Identifier())
	}
	if err != nil {
		clear()
		return nil, manager, nil, err
	}
	return service, manager, clear, nil
}

func assertOnlyLLMGWProvider(t *testing.T, manager *sdkaccess.Manager) {
	t.Helper()
	providers := manager.Providers()
	if len(providers) != 1 || providers[0].Identifier() != AccessProviderType {
		identifiers := make([]string, 0, len(providers))
		for _, provider := range providers {
			identifiers = append(identifiers, provider.Identifier())
		}
		t.Fatalf("access providers = %v, want only %q", identifiers, AccessProviderType)
	}
}

func assertExclusiveDuringReloadWindow(t *testing.T, manager *sdkaccess.Manager) {
	t.Helper()
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		providers := manager.Providers()
		if len(providers) != 1 || providers[0].Identifier() != AccessProviderType {
			identifiers := make([]string, 0, len(providers))
			for _, provider := range providers {
				identifiers = append(identifiers, provider.Identifier())
			}
			t.Fatalf("access providers after config rewrite = %v, want only %q", identifiers, AccessProviderType)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func callbackRequest(t *testing.T, client *http.Client, baseURL string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodGet,
		baseURL+"?code="+callbackCode+"&state="+callbackState,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func waitForSDK(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("SDK did not become ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForLog(t *testing.T, logs *lockedBuffer, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), message) {
		if time.Now().After(deadline) {
			t.Fatalf("log %q did not appear:\n%s", message, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func captureLogrus(t *testing.T, output *lockedBuffer) func() {
	t.Helper()
	previousOutput := log.StandardLogger().Out
	previousFormatter := log.StandardLogger().Formatter
	previousLevel := log.GetLevel()
	log.SetOutput(output)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true})
	log.SetLevel(log.DebugLevel)
	return func() {
		log.SetOutput(previousOutput)
		log.SetFormatter(previousFormatter)
		log.SetLevel(previousLevel)
	}
}

type sdkFixture struct {
	config     *sdkconfig.Config
	configPath string
	authDir    string
	baseURL    string
}

func newSDKFixture(t *testing.T) sdkFixture {
	t.Helper()
	port := freePort(t)
	root := t.TempDir()
	fixture := sdkFixture{
		configPath: filepath.Join(root, "config.yaml"),
		authDir:    filepath.Join(root, "auth"),
		baseURL:    fmt.Sprintf("http://127.0.0.1:%d", port),
	}
	if err := os.Mkdir(fixture.authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rewriteSDKConfig(t, fixture, false)
	cfg, err := sdkconfig.LoadConfig(fixture.configPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.config = cfg
	return fixture
}

func rewriteSDKConfig(t *testing.T, fixture sdkFixture, nativeAPIKey bool) {
	t.Helper()
	apiKeys := ""
	pluginsEnabled := false
	if nativeAPIKey {
		apiKeys = "api-keys:\n  - native-bypass\n"
		pluginsEnabled = true
	}
	body := fmt.Sprintf(
		"host: 127.0.0.1\nport: %s\nauth-dir: %s\ndisable-image-generation: true\n%s"+
			"remote-management:\n  allow-remote: false\n  secret-key: \"\"\n  disable-control-panel: true\n"+
			"plugins:\n  enabled: %t\n",
		strings.TrimPrefix(fixture.baseURL, "http://127.0.0.1:"),
		fixture.authDir,
		apiKeys,
		pluginsEnabled,
	)
	if err := os.WriteFile(fixture.configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

type rogueAccessProvider struct {
	calls atomic.Int64
}

func (*rogueAccessProvider) Identifier() string {
	return "rogue"
}

func (p *rogueAccessProvider) Authenticate(
	context.Context,
	*http.Request,
) (*sdkaccess.Result, *sdkaccess.AuthError) {
	p.calls.Add(1)
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: "rogue"}, nil
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
