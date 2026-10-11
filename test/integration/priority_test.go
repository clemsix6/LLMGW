package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/clemsix6/LLMGW/internal/command"
)

// priorityGeneration sends one generation whose content is unique, so session
// affinity never pins two requests of this test to the same account.
func priorityGeneration(t *testing.T, plaintext string, sequence int) int {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"priority-fixture-%d-%s"}]}`,
		priorityModel, sequence, t.Name())
	status, response := gatewayRequest(t, http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(body), requestHeaders{authorization: "Bearer " + plaintext})
	if status != http.StatusOK && status != http.StatusTooManyRequests {
		t.Logf("priority generation body: %s", safeBodySummary(response))
	}
	return status
}

// servedBy returns the credentials the stub saw after the first already requests.
func servedBy(t *testing.T, already int) []string {
	t.Helper()
	all := testHarness.Upstream.Authorizations()
	if len(all) < already {
		t.Fatalf("stub saw %d requests, want at least %d", len(all), already)
	}
	return all[already:]
}

// TestPriorityAccountTakesTrafficThenFallsBack proves the highest tier serves
// every request, and once it is rate limited the remaining tier alternates.
func TestPriorityAccountTakesTrafficThenFallsBack(t *testing.T) {
	created := testHarness.createKey(t, "priority-routing")
	start := len(testHarness.Upstream.Authorizations())

	const prioritized = 4
	for i := 0; i < prioritized; i++ {
		testHarness.Upstream.Enqueue(priorityUsageResponse())
		if status := priorityGeneration(t, created.Plaintext, i); status != http.StatusOK {
			t.Fatalf("prioritized request %d status = %d, want 200", i, status)
		}
	}
	for i, token := range servedBy(t, start) {
		if token != "Bearer "+priorityMainToken {
			t.Fatalf("request %d served by %q, want the priority account", i, token)
		}
	}

	// The priority account answers 429; the SDK retries the same call on another
	// account, which serves it. The 429 carries a Retry-After, so the cooldown outlasts the rest of the test.
	testHarness.Upstream.Enqueue(priorityRateLimit(), priorityUsageResponse())
	if status := priorityGeneration(t, created.Plaintext, prioritized); status != http.StatusOK {
		t.Fatalf("failover request status = %d, want 200", status)
	}
	afterFailover := len(testHarness.Upstream.Authorizations())

	const fallback = 4
	for i := 0; i < fallback; i++ {
		testHarness.Upstream.Enqueue(priorityUsageResponse())
		if status := priorityGeneration(t, created.Plaintext, prioritized+1+i); status != http.StatusOK {
			t.Fatalf("fallback request %d status = %d, want 200", i, status)
		}
	}
	assertAlternatingSpares(t, servedBy(t, afterFailover))
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// assertAlternatingSpares requires only the two spare accounts, one after the other.
func assertAlternatingSpares(t *testing.T, tokens []string) {
	t.Helper()
	spares := map[string]bool{"Bearer " + prioritySpareAToken: true, "Bearer " + prioritySpareBToken: true}
	for i, token := range tokens {
		if !spares[token] {
			t.Fatalf("fallback request %d served by %q, want a spare account", i, token)
		}
		if i > 0 && token == tokens[i-1] {
			t.Fatalf("fallback requests %d and %d both served by %q, want alternation", i-1, i, token)
		}
	}
}

// priorityCommandEnv is a local operator environment: a config whose auth
// directory holds one credential, and the database of the running harness.
type priorityCommandEnv struct {
	configPath string
	authDir    string
	getenv     func(string) string
}

// newPriorityCommandEnv builds an operator configuration separate from the
// running gateway's, so the command never rewrites the live auth directory.
func newPriorityCommandEnv(t *testing.T) priorityCommandEnv {
	t.Helper()
	dsn, err := testHarness.container.ConnectionString(context.Background(), "sslmode=disable")
	if err != nil {
		t.Fatalf("resolve operator database: %v", err)
	}
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatalf("create operator auth dir: %v", err)
	}
	if err := writePriorityAuthFiles(authDir, testHarness.Upstream.URL()); err != nil {
		t.Fatalf("write operator auth files: %v", err)
	}
	config := fmt.Sprintf(`
auth-dir: %q
disable-image-generation: true
request-retry: 0
max-retry-credentials: 1
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
home:
  enabled: false
pprof:
  enable: false
routing:
  session-affinity: false
llmgw:
  postgres-dsn-env: OPERATOR_DSN
  key-pepper-env: OPERATOR_PEPPER
  usage-retention-days: 35
  usage-outstanding-capacity: 2
`, authDir)
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write operator config: %v", err)
	}
	return priorityCommandEnv{configPath: configPath, authDir: authDir, getenv: func(name string) string {
		switch name {
		case "OPERATOR_DSN":
			return dsn
		case "OPERATOR_PEPPER":
			return "operator-key-pepper-32-bytes!!!!!"
		}
		return ""
	}}
}

// run executes one auth priority command and returns its stdout and error.
func (e priorityCommandEnv) run(t *testing.T, id, value string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := command.Run(context.Background(),
		[]string{"--config", e.configPath, "auth", "priority", id, value},
		command.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr, Getenv: e.getenv})
	return stdout.String(), err
}

// priorityOf reads one credential's priority back from the auth directory.
func (e priorityCommandEnv) priorityOf(t *testing.T, id string) int {
	t.Helper()
	auths, err := cliproxy.ListAuth(context.Background(), e.authDir)
	if err != nil {
		t.Fatalf("list operator auths: %v", err)
	}
	for _, auth := range auths {
		if auth.ID == id {
			return auth.Priority
		}
	}
	t.Fatalf("credential %q not found", id)
	return 0
}

// TestAuthPriorityCommandRefusesWhileServing proves the command writes nothing
// while a server holds the serve lock, and writes once the lock is free.
func TestAuthPriorityCommandRefusesWhileServing(t *testing.T) {
	env := newPriorityCommandEnv(t)
	lock, err := testHarness.Store.AcquireServeLock(context.Background())
	if err != nil {
		t.Fatalf("hold the serve lock: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = lock.Release(context.Background())
		}
	}()

	stdout, err := env.run(t, prioritySpareAFile, "5")
	if err == nil || !strings.Contains(err.Error(), "stop the server") {
		t.Fatalf("command error = %v, want the stop-the-server refusal", err)
	}
	if stdout != "" {
		t.Fatalf("refused command printed %q, want nothing", stdout)
	}
	if got := env.priorityOf(t, prioritySpareAFile); got != 0 {
		t.Fatalf("priority after refusal = %d, want it unchanged", got)
	}

	if err := lock.Release(context.Background()); err != nil {
		t.Fatalf("release the serve lock: %v", err)
	}
	released = true

	stdout, err = env.run(t, prioritySpareAFile, "5")
	if err != nil {
		t.Fatalf("command without a server: %v", err)
	}
	if !strings.Contains(stdout, "id\t"+prioritySpareAFile+"\n") || !strings.Contains(stdout, "priority\t5\n") {
		t.Fatalf("command output = %q, want the updated record", stdout)
	}
	if got := env.priorityOf(t, prioritySpareAFile); got != 5 {
		t.Fatalf("priority after write = %d, want 5", got)
	}

	if _, err := env.run(t, prioritySpareAFile, "0"); err != nil {
		t.Fatalf("clear priority: %v", err)
	}
	if got := env.priorityOf(t, prioritySpareAFile); got != 0 {
		t.Fatalf("priority after clear = %d, want 0", got)
	}
}
