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
	status     int           // status is the HTTP status the failure reports
	retryAfter time.Duration // retryAfter is the cooldown hint attached to the failure
}

// Error implements error.
func (e *stubUpstreamError) Error() string { return "upstream failure" }

// StatusCode reports the HTTP status the SDK classifies the failure by.
func (e *stubUpstreamError) StatusCode() int { return e.status }

// RetryAfter reports the cooldown hint the SDK honours for transient failures.
func (e *stubUpstreamError) RetryAfter() *time.Duration { return &e.retryAfter }

// recordingExecutor records which auth served each call and can fail chosen auths.
type recordingExecutor struct {
	mu      sync.Mutex                    // mu guards served and failing
	served  []string                      // served lists the auth IDs that answered, in order
	failing map[string]*stubUpstreamError // failing maps an auth ID to the failure it returns
}

// Identifier returns the provider the executor serves.
func (e *recordingExecutor) Identifier() string { return priorityTestProvider }

// Execute records the serving auth, or returns the failure configured for it.
func (e *recordingExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ sdkexecutor.Request, _ sdkexecutor.Options) (sdkexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if failure := e.failing[auth.ID]; failure != nil {
		return sdkexecutor.Response{}, failure
	}
	e.served = append(e.served, auth.ID)
	return sdkexecutor.Response{Payload: []byte("{}")}, nil
}

// ExecuteStream is unused by these tests.
func (e *recordingExecutor) ExecuteStream(context.Context, *coreauth.Auth, sdkexecutor.Request, sdkexecutor.Options) (*sdkexecutor.StreamResult, error) {
	return nil, nil
}

// Refresh returns the auth unchanged.
func (e *recordingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

// CountTokens is unused by these tests.
func (e *recordingExecutor) CountTokens(context.Context, *coreauth.Auth, sdkexecutor.Request, sdkexecutor.Options) (sdkexecutor.Response, error) {
	return sdkexecutor.Response{}, nil
}

// HttpRequest is unused by these tests.
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
	manager  *coreauth.Manager  // manager is the SDK manager under test
	executor *recordingExecutor // executor records which auth served each call
	priority string             // priority is the ID of the prioritised credential
	others   []string           // others are the IDs of the unprioritised credentials
}

// newPriorityFixture writes the three credential files into dir and loads a manager
// over store that routes with the given selector.
func newPriorityFixture(t *testing.T, store coreauth.Store, dir string, selector coreauth.Selector) *priorityFixture {
	t.Helper()
	priority := writeClaudeAuthFile(t, dir, "main", map[string]any{"priority": 10})
	others := []string{
		writeClaudeAuthFile(t, dir, "spare-a", nil),
		writeClaudeAuthFile(t, dir, "spare-b", nil),
	}
	executor := &recordingExecutor{failing: map[string]*stubUpstreamError{}}
	manager := coreauth.NewManager(store, selector, nil)
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
	return f.callSession(t, "")
}

// callSession drives one request of the given conversation and returns the auth that served it.
// An empty session sends no conversation identity.
func (f *priorityFixture) callSession(t *testing.T, session string) string {
	t.Helper()
	request := sdkexecutor.Request{Model: priorityTestModel, Payload: []byte("{}")}
	options := sdkexecutor.Options{}
	if session != "" {
		options.Metadata = map[string]any{sdkexecutor.DerivedSessionIDMetadataKey: session}
	}
	if _, err := f.manager.Execute(context.Background(), []string{priorityTestProvider}, request, options); err != nil {
		t.Fatalf("execute: %v", err)
	}
	served := f.executor.take()
	if len(served) != 1 {
		t.Fatalf("served = %v, want exactly one serving auth", served)
	}
	return served[0]
}

// TestServingStoreRoutesToPriorityAccount requires every call to land on the priority account.
func TestServingStoreRoutesToPriorityAccount(t *testing.T) {
	dir := t.TempDir()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir, &coreauth.RoundRobinSelector{})

	for i := 0; i < 6; i++ {
		if got := fixture.call(t); got != fixture.priority {
			t.Fatalf("call %d served by %q, want priority account %q", i, got, fixture.priority)
		}
	}
}

// TestServingStoreFallsBackToRoundRobinWhileRateLimited requires the lower tier to serve while the priority account is rate limited.
func TestServingStoreFallsBackToRoundRobinWhileRateLimited(t *testing.T) {
	dir := t.TempDir()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir, &coreauth.RoundRobinSelector{})

	// Round-robin over every account could not keep a run of calls on one of them.
	for i := 0; i < 4; i++ {
		if got := fixture.call(t); got != fixture.priority {
			t.Fatalf("call %d before the failure served by %q, want priority account %q", i, got, fixture.priority)
		}
	}
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

// TestServingStoreReturnsToPriorityAccountAfterCooldown requires the priority account to resume
// after a cooldown. The SDK floors 429 cooldowns well above a test-friendly wait, so recovery is driven through a
// transient server error, which honours the retry-after hint exactly and takes the same
// availability path back to the highest tier.
func TestServingStoreReturnsToPriorityAccountAfterCooldown(t *testing.T) {
	dir := t.TempDir()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir, &coreauth.RoundRobinSelector{})
	fixture.executor.setFailure(fixture.priority, &stubUpstreamError{status: http.StatusServiceUnavailable, retryAfter: priorityTestCooldown})

	if got := fixture.call(t); got == fixture.priority {
		t.Fatalf("failing priority account served the call")
	}
	fixture.executor.setFailure(fixture.priority, nil)
	if got := fixture.call(t); got == fixture.priority {
		t.Fatalf("priority account served the call during its cooldown")
	}

	time.Sleep(priorityTestCooldown + 300*time.Millisecond)
	// Round-robin could not keep a run of calls on one account.
	for i := 0; i < 4; i++ {
		if got := fixture.call(t); got != fixture.priority {
			t.Fatalf("call %d after cooldown served by %q, want priority account %q", i, got, fixture.priority)
		}
	}
}

// TestServingStoreSessionAffinityKeepsBindingAfterRecovery covers session affinity: with it on, as shipped, a conversation stays on its bound account while that
// account remains available, so only new conversations return to the recovered priority account.
func TestServingStoreSessionAffinityKeepsBindingAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	// The SDK's own public constructor; the configuration path that builds it is unexported.
	affinity := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer affinity.Stop()
	fixture := newPriorityFixture(t, newServingTokenStoreFor(dir), dir, affinity)

	if got := fixture.callSession(t, "conversation-a"); got != fixture.priority {
		t.Fatalf("conversation A started on %q, want priority account %q", got, fixture.priority)
	}
	fixture.executor.setFailure(fixture.priority, &stubUpstreamError{status: http.StatusServiceUnavailable, retryAfter: priorityTestCooldown})
	spareA := fixture.callSession(t, "conversation-a")
	if spareA == fixture.priority {
		t.Fatalf("conversation A stayed on the failing priority account")
	}
	fixture.executor.setFailure(fixture.priority, nil)
	spareB := fixture.callSession(t, "conversation-b")
	if spareB == fixture.priority {
		t.Fatalf("conversation B landed on the priority account during its cooldown")
	}

	time.Sleep(priorityTestCooldown + 300*time.Millisecond)
	if got := fixture.callSession(t, "conversation-a"); got != spareA {
		t.Fatalf("conversation A after recovery on %q, want its bound %q", got, spareA)
	}
	if got := fixture.callSession(t, "conversation-b"); got != spareB {
		t.Fatalf("conversation B after recovery on %q, want its bound %q", got, spareB)
	}
	for _, session := range []string{"conversation-c", "conversation-d", "conversation-e", "conversation-f"} {
		if got := fixture.callSession(t, session); got != fixture.priority {
			t.Fatalf("new %s on %q, want priority account %q", session, got, fixture.priority)
		}
	}
}

// TestServingStoreKeepsPriorityAcrossCredentialRewrite requires a rewritten credential file to keep its priority.
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

	stored := readAuthJSON(t, dir, name)
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
