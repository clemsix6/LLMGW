package cliproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdkproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	priorityTestProvider = "claude"
	priorityTestModel    = "claude-priority-test"
	priorityTestCooldown = time.Second
)

// stubUpstreamError is an upstream failure carrying the retry-after hint the SDK turns into a cooldown.
type stubUpstreamError struct {
	status     int
	retryAfter time.Duration
}

func (e *stubUpstreamError) Error() string { return "upstream failure" }

func (e *stubUpstreamError) StatusCode() int { return e.status }

func (e *stubUpstreamError) RetryAfter() *time.Duration { return &e.retryAfter }

// recordingExecutor records which auth served each call and can fail chosen auths.
type recordingExecutor struct {
	mu      sync.Mutex
	served  []string
	failing map[string]*stubUpstreamError
}

func (e *recordingExecutor) Identifier() string { return priorityTestProvider }

func (e *recordingExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ sdkexecutor.Request, _ sdkexecutor.Options) (sdkexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if failure := e.failing[auth.ID]; failure != nil {
		return sdkexecutor.Response{}, failure
	}
	e.served = append(e.served, auth.ID)
	return sdkexecutor.Response{Payload: []byte("{}")}, nil
}

func (e *recordingExecutor) ExecuteStream(context.Context, *coreauth.Auth, sdkexecutor.Request, sdkexecutor.Options) (*sdkexecutor.StreamResult, error) {
	return nil, nil
}

func (e *recordingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *recordingExecutor) CountTokens(context.Context, *coreauth.Auth, sdkexecutor.Request, sdkexecutor.Options) (sdkexecutor.Response, error) {
	return sdkexecutor.Response{}, nil
}

func (e *recordingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// setFailure makes one auth fail with the given error, or succeed again when failure is nil.
func (e *recordingExecutor) setFailure(id string, failure *stubUpstreamError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failing[id] = failure
}

// take returns and clears the recorded serving order.
func (e *recordingExecutor) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	served := e.served
	e.served = nil
	return served
}

// writeClaudeAuthFile writes a minimal Claude credential file and returns its name.
func writeClaudeAuthFile(t *testing.T, dir, email string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"type": "claude", "email": email, "access_token": "dummy-" + email}
	for key, value := range extra {
		body[key] = value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	name := "claude-" + email + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return name
}

// priorityFixture is a manager loaded from three Claude files, one of them prioritised.
type priorityFixture struct {
	manager  *coreauth.Manager
	executor *recordingExecutor
	priority string
	others   []string
}

func newPriorityFixture(t *testing.T, store coreauth.Store, dir string) *priorityFixture {
	t.Helper()
	priority := writeClaudeAuthFile(t, dir, "main", map[string]any{"priority": 10})
	others := []string{
		writeClaudeAuthFile(t, dir, "spare-a", nil),
		writeClaudeAuthFile(t, dir, "spare-b", nil),
	}
	executor := &recordingExecutor{failing: map[string]*stubUpstreamError{}}
	manager := coreauth.NewManager(store, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("load manager: %v", err)
	}
	registerTestModels(t, manager)
	return &priorityFixture{manager: manager, executor: executor, priority: priority, others: others}
}

// call drives one request through the manager and returns the auth that served it.
func (f *priorityFixture) call(t *testing.T) string {
	t.Helper()
	request := sdkexecutor.Request{Model: priorityTestModel, Payload: []byte("{}")}
	if _, err := f.manager.Execute(context.Background(), []string{priorityTestProvider}, request, sdkexecutor.Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	served := f.executor.take()
	if len(served) != 1 {
		t.Fatalf("served = %v, want exactly one serving auth", served)
	}
	return served[0]
}

func TestServingStoreRoutesToPriorityAccount(t *testing.T) {
	dir := t.TempDir()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir)

	for i := 0; i < 6; i++ {
		if got := fixture.call(t); got != fixture.priority {
			t.Fatalf("call %d served by %q, want priority account %q", i, got, fixture.priority)
		}
	}
}

func TestServingStoreFallsBackToRoundRobinWhileRateLimited(t *testing.T) {
	dir := t.TempDir()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir)
	fixture.executor.setFailure(fixture.priority, &stubUpstreamError{status: http.StatusTooManyRequests, retryAfter: time.Minute})

	// The rate-limited priority attempt falls through to a lower tier within the same call.
	first := fixture.call(t)
	if first == fixture.priority {
		t.Fatalf("rate-limited priority account served the call")
	}

	// While the priority account cools down, the remaining tier alternates.
	served := []string{first}
	for i := 0; i < 3; i++ {
		served = append(served, fixture.call(t))
	}
	for i, id := range served {
		if id == fixture.priority || (i > 0 && id == served[i-1]) {
			t.Fatalf("fallback order = %v, want alternation between %v", served, fixture.others)
		}
	}
}

// The SDK floors 429 cooldowns well above a test-friendly wait, so recovery is driven through a
// transient server error, which honours the retry-after hint exactly and takes the same
// availability path back to the highest tier.
func TestServingStoreReturnsToPriorityAccountAfterCooldown(t *testing.T) {
	dir := t.TempDir()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir)
	fixture.executor.setFailure(fixture.priority, &stubUpstreamError{status: http.StatusServiceUnavailable, retryAfter: priorityTestCooldown})

	if got := fixture.call(t); got == fixture.priority {
		t.Fatalf("failing priority account served the call")
	}
	fixture.executor.setFailure(fixture.priority, nil)
	if got := fixture.call(t); got == fixture.priority {
		t.Fatalf("priority account served the call during its cooldown")
	}

	time.Sleep(priorityTestCooldown + 300*time.Millisecond)
	if got := fixture.call(t); got != fixture.priority {
		t.Fatalf("after cooldown served by %q, want priority account %q", got, fixture.priority)
	}
}

func TestServingStoreKeepsPriorityAcrossCredentialRewrite(t *testing.T) {
	dir := t.TempDir()
	name := writeClaudeAuthFile(t, dir, "main", map[string]any{"priority": 7})
	store := newServingTokenStoreFor(dir)

	auths, err := store.List(context.Background())
	if err != nil || len(auths) != 1 {
		t.Fatalf("list = %v, %v", auths, err)
	}
	auths[0].Metadata["access_token"] = "refreshed"
	if _, err := store.Save(context.Background(), auths[0]); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode auth file: %v", err)
	}
	if stored["priority"] != float64(7) || stored["access_token"] != "refreshed" {
		t.Fatalf("rewritten file = %v, want priority kept and token refreshed", stored)
	}
}

// newServingTokenStoreFor builds the serving store rooted at one directory.
func newServingTokenStoreFor(dir string) coreauth.Store {
	store := newServingTokenStore()
	store.SetBaseDir(dir)
	return store
}

// registerTestModels exposes the test model for every loaded auth, as the SDK service does at startup.
func registerTestModels(t *testing.T, manager *coreauth.Manager) {
	t.Helper()
	registry := sdkproxy.GlobalModelRegistry()
	for _, auth := range manager.List() {
		id := auth.ID
		registry.RegisterClient(id, priorityTestProvider, []*sdkproxy.ModelInfo{{ID: priorityTestModel}})
		t.Cleanup(func() { registry.UnregisterClient(id) })
	}
}
