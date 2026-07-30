package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/projectkey"
	"github.com/jackc/pgx/v5/pgxpool"
	logrus "github.com/sirupsen/logrus"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	harnessStartupTimeout  = 90 * time.Second
	harnessShutdownTimeout = 35 * time.Second
)

// Fixture secrets the harness registers so no response may ever echo them back.
const (
	upstreamFailureSecret = "upstream-failure-body-secret"
	upstreamHeaderSecret  = "upstream-response-header-secret"
	fixtureToolSecret     = "fixture-tool-payload-secret"
	runtimeAccountSecret  = "runtime-added-account-secret"
)

// Harness owns the single process-wide embedded SDK integration environment.
type Harness struct {
	BaseURL    string              // BaseURL is the embedded proxy URL.
	ConfigPath string              // ConfigPath is the shared YAML path.
	AuthDir    string              // AuthDir is the startup-only SDK auth directory.
	Store      *postgres.Store     // Store persists governance state.
	Keys       *projectkey.Service // Keys creates and authenticates project keys.
	Upstream   *StubUpstream       // Upstream is the deterministic provider.
	Usage      *usageCapture       // Usage observes SDK principal propagation.

	cancel context.CancelFunc // cancel stops the single SDK service.
	done   <-chan error       // done reports the SDK Run result.

	client                 *http.Client                  // client drives the embedded proxy.
	container              *tcpostgres.PostgresContainer // container owns PostgreSQL 16.
	db                     *pgxpool.Pool                 // db supports integration assertions.
	root                   string                        // root holds temporary config and auth files.
	logs                   *lockedBuffer                 // logs captures process logging safely.
	secretMu               sync.Mutex                    // secretMu protects the shutdown leak registry.
	secrets                map[string]struct{}           // secrets holds every sensitive Task 11 fixture.
	closeOnce              sync.Once                     // closeOnce enforces one SDK shutdown.
	closeResult            harnessCloseResult            // closeResult retains independently audited cleanup.
	closeErr               error                         // closeErr retains the shutdown result.
	stopOnce               sync.Once                     // stopOnce enforces one service stop.
	stopErr                error                         // stopErr retains the service stop result.
	keyID                  atomic.Uint64                 // keyID makes project labels unique.
	resourceCleanupFailure error                         // resourceCleanupFailure injects a test-only local failure.
}

// harnessCloseResult preserves each cleanup component for isolated error validation.
type harnessCloseResult struct {
	serviceErr   error // serviceErr reports the embedded lifecycle result.
	logErr       error // logErr reports registered-secret leakage.
	localErr     error // localErr reports local pool/upstream cleanup failure.
	containerErr error // containerErr reports PostgreSQL container cleanup failure.
	tempErr      error // tempErr reports temporary-directory cleanup failure.
}

// joined returns the normal TestMain-compatible aggregate cleanup error.
func (r harnessCloseResult) joined() error {
	return errors.Join(r.serviceErr, r.logErr, r.localErr, r.containerErr, r.tempErr)
}

// NewHarness starts PostgreSQL, the upstream, and exactly one embedded SDK service.
func NewHarness() (_ *Harness, returnErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), harnessStartupTimeout)
	defer cancel()

	harness := &Harness{
		client:  &http.Client{Timeout: 5 * time.Second},
		logs:    &lockedBuffer{},
		secrets: make(map[string]struct{}),
		Usage:   newUsageCapture(),
	}
	defer func() {
		if returnErr != nil {
			_ = harness.Close()
		}
	}()
	harness.captureLogs()

	if err := harness.startUpstreamAndFiles(); err != nil {
		return nil, err
	}
	if err := harness.startPostgres(ctx); err != nil {
		return nil, err
	}
	if err := harness.startService(ctx); err != nil {
		return nil, err
	}
	return harness, nil
}

// startUpstreamAndFiles creates the deterministic provider and shared YAML.
func (h *Harness) startUpstreamAndFiles() error {
	h.Upstream = NewStubUpstream()
	h.registerSecrets(
		"upstream-account-a", "upstream-account-b",
		"upstream-codex-account-a", "upstream-codex-account-b",
		"fixture-prompt", fixtureToolSecret, h.Upstream.URL(),
		upstreamFailureSecret, upstreamHeaderSecret, "transient-failover-fixture",
		"cooling-fixture", runtimeAccountSecret, "native-bypass", "attempted-management-key",
	)

	root, err := os.MkdirTemp("", "llmgw-integration-")
	if err != nil {
		return fmt.Errorf("create integration directory:\n%w", err)
	}
	h.root = root
	h.AuthDir = filepath.Join(root, "auth")
	if err := os.Mkdir(h.AuthDir, 0o700); err != nil {
		return fmt.Errorf("create integration auth directory:\n%w", err)
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	h.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	h.ConfigPath = filepath.Join(root, "config.yaml")
	if err := os.WriteFile(h.ConfigPath, h.configYAML(port), 0o600); err != nil {
		return fmt.Errorf("write integration configuration:\n%w", err)
	}
	return nil
}

// startPostgres starts PostgreSQL 16 and opens governance and assertion pools.
func (h *Harness) startPostgres(ctx context.Context) error {
	container, err := tcpostgres.Run(
		ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		return fmt.Errorf("start integration PostgreSQL:\n%w", err)
	}
	h.container = container

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("resolve integration PostgreSQL connection:\n%w", err)
	}
	return h.openPostgres(ctx, dsn)
}

// openPostgres opens governance and assertion pools against the container.
func (h *Harness) openPostgres(ctx context.Context, dsn string) error {
	migrationStore, err := postgres.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open integration governance store:\n%w", err)
	}
	migrationStore.Close()
	h.db, err = pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open integration assertion pool:\n%w", err)
	}
	if err := h.db.Ping(ctx); err != nil {
		return fmt.Errorf("ping integration assertion pool:\n%w", err)
	}
	const serviceRole = `
CREATE ROLE llmgw_service LOGIN PASSWORD 'task11-service-role-password'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT;
GRANT CONNECT ON DATABASE llmgw TO llmgw_service;
GRANT USAGE, CREATE ON SCHEMA public TO llmgw_service;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO llmgw_service;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO llmgw_service`
	if _, err := h.db.Exec(ctx, serviceRole); err != nil {
		return fmt.Errorf("create integration service role:\n%w", err)
	}
	serviceURL, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse integration service DSN:\n%w", err)
	}
	serviceURL.User = url.UserPassword("llmgw_service", "task11-service-role-password")
	h.Store, err = postgres.New(ctx, serviceURL.String())
	if err != nil {
		return fmt.Errorf("open non-owner integration governance store:\n%w", err)
	}
	h.registerSecrets("task11-service-role-password")
	return nil
}

// startService loads the shared config and starts the only SDK lifecycle.
func (h *Harness) startService(ctx context.Context) error {
	cfg, err := config.Load(h.ConfigPath, func(string) string { return "" })
	if err != nil {
		return fmt.Errorf("load integration configuration:\n%w", err)
	}
	h.Keys, err = projectkey.NewService(
		h.Store,
		[]byte("integration-key-pepper-32-bytes!!"),
		rand.Reader,
		time.Now,
	)
	if err != nil {
		return fmt.Errorf("construct integration key service:\n%w", err)
	}
	usageBridge, err := cliproxy.NewUsageBridge(
		rand.Reader,
		cfg.LLMGW.UsageOutstandingCapacity,
	)
	if err != nil {
		return fmt.Errorf("construct integration usage bridge:\n%w", err)
	}
	middleware := cliproxy.NewMiddleware(h.Keys, h.Store, time.Now, usageBridge,
		nil, // alerting tracker: wired in a later batch
	)
	usagePlugin := cliproxy.NewUsagePlugin(h.Store, usageBridge, postgres.IsTransientUsageError)
	service, err := cliproxy.NewService(cfg, middleware, usagePlugin, h.Usage)
	if err != nil {
		return fmt.Errorf("construct integration proxy:\n%w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	h.cancel = cancel
	h.done = done
	go func() {
		done <- service.Run(runCtx)
	}()
	if err := h.waitReady(ctx); err != nil {
		cancel()
		return err
	}
	return nil
}

// configYAML renders the real SDK OpenAI-compatible provider configuration.
func (h *Harness) configYAML(port int) []byte {
	return []byte(fmt.Sprintf(`
host: 127.0.0.1
port: %d
auth-dir: %q
disable-image-generation: true
request-log: false
debug: false
request-retry: 0
max-retry-credentials: 2
transient-error-cooldown-seconds: -1
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
home:
  enabled: false
pprof:
  enable: false
plugins:
  enabled: false
routing:
  strategy: round-robin
  session-affinity: false
openai-compatibility:
  - name: integration
    base-url: %q
    api-key-entries:
      - api-key: upstream-account-a
      - api-key: upstream-account-b
    models:
      - name: upstream-model
        alias: test-model
        force-mapping: true
      - name: unpriced-upstream-model
        alias: unpriced-model
        force-mapping: true
codex-api-key:
  - api-key: upstream-codex-account-a
    base-url: %q
    models:
      - name: codex-upstream-model
        alias: multi-attempt-model
        force-mapping: true
      - name: codex-cooldown-model
        alias: cooldown-model
        force-mapping: true
      - name: codex-other-model
        alias: cooldown-other-model
        force-mapping: true
      - name: codex-no-reset-model
        alias: cooldown-no-reset-model
        force-mapping: true
  - api-key: upstream-codex-account-b
    base-url: %q
    models:
      - name: codex-upstream-model
        alias: multi-attempt-model
        force-mapping: true
      - name: codex-cooldown-model
        alias: cooldown-model
        force-mapping: true
      - name: codex-other-model
        alias: cooldown-other-model
        force-mapping: true
      - name: codex-no-reset-model
        alias: cooldown-no-reset-model
        force-mapping: true
llmgw:
  postgres-dsn-env: TEST_POSTGRES_DSN
  key-pepper-env: TEST_KEY_PEPPER
  usage-retention-days: 35
  usage-outstanding-capacity: 2
`, port, h.AuthDir, h.Upstream.URL()+"/v1", h.Upstream.URL(), h.Upstream.URL()))
}

// waitReady polls the public health endpoint without credentials.
func (h *Harness) waitReady(ctx context.Context) error {
	var lastErr error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/healthz", nil)
		if err != nil {
			return fmt.Errorf("create integration health request:\n%w", err)
		}
		response, err := h.client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health status %d", response.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for embedded proxy readiness:\n%w", errors.Join(ctx.Err(), lastErr))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// countRequests returns persisted request rows for one operation and optional path.
func (h *Harness) countRequests(
	t *testing.T,
	projectID int64,
	operation governance.Operation,
	path string,
) int64 {
	t.Helper()
	const query = `
SELECT count(*)
FROM request_event
WHERE project_id = $1
  AND operation = $2
  AND ($3 = '' OR path = $3)`
	var count int64
	if err := h.db.QueryRow(context.Background(), query, projectID, operation, path).Scan(&count); err != nil {
		t.Fatal("count integration requests failed")
	}
	return count
}

// assertSecretsAbsent checks captured logs without echoing the searched values.
func (h *Harness) assertSecretsAbsent(t *testing.T, secrets ...string) {
	t.Helper()
	h.registerSecrets(secrets...)
	for _, secret := range secrets {
		if secret != "" && h.logs.Contains(secret) {
			t.Fatal("integration logs contain a registered secret")
		}
	}
}

// registerSecrets adds dynamic and fixed sensitive fixtures to the shutdown audit.
func (h *Harness) registerSecrets(secrets ...string) {
	h.secretMu.Lock()
	defer h.secretMu.Unlock()
	if h.secrets == nil {
		h.secrets = make(map[string]struct{})
	}
	for _, secret := range secrets {
		if secret != "" {
			h.secrets[secret] = struct{}{}
		}
	}
}

// captureLogs captures standard and SDK logs in a concurrency-safe buffer.
func (h *Harness) captureLogs() {
	log.SetOutput(h.logs)
	logrus.SetOutput(h.logs)
}

// Close stops the one SDK lifecycle before closing PostgreSQL and the upstream.
func (h *Harness) Close() error {
	_ = h.closeComponents()
	return h.closeErr
}

// closeComponents runs cleanup once and retains every component independently.
func (h *Harness) closeComponents() harnessCloseResult {
	h.closeOnce.Do(func() {
		h.closeResult = harnessCloseResult{
			serviceErr:   h.stopService(),
			logErr:       h.checkLogLeaks(),
			localErr:     h.closeLocalResources(),
			containerErr: h.closeContainer(),
			tempErr:      h.removeTemporaryFiles(),
		}
		h.closeErr = h.closeResult.joined()
	})
	return h.closeResult
}

// checkLogLeaks rejects every registered sensitive fixture without echoing it.
func (h *Harness) checkLogLeaks() error {
	if h.logs == nil {
		return nil
	}
	h.secretMu.Lock()
	secrets := make([]string, 0, len(h.secrets))
	for secret := range h.secrets {
		secrets = append(secrets, secret)
	}
	h.secretMu.Unlock()
	for _, secret := range secrets {
		if h.logs.Contains(secret) {
			return errors.New("integration logs contain a registered secret")
		}
	}
	return nil
}

// closeLocalResources closes in-process database and HTTP clients.
func (h *Harness) closeLocalResources() error {
	if h.db != nil {
		h.db.Close()
	}
	if h.Store != nil {
		h.Store.Close()
	}
	if h.Upstream != nil {
		h.Upstream.Close()
	}
	return h.resourceCleanupFailure
}

// closeContainer removes the external PostgreSQL container.
func (h *Harness) closeContainer() error {
	if h.container != nil {
		if err := h.container.Terminate(context.Background()); err != nil {
			return fmt.Errorf("terminate integration PostgreSQL:\n%w", err)
		}
	}
	return nil
}

// removeTemporaryFiles removes the integration configuration and auth directory.
func (h *Harness) removeTemporaryFiles() error {
	if h.root != "" {
		if err := os.RemoveAll(h.root); err != nil {
			return fmt.Errorf("remove integration directory:\n%w", err)
		}
	}
	return nil
}

// freePort reserves and releases one loopback port for the SDK listener.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("choose integration port:\n%w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
