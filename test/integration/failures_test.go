package integration

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

const persistenceChildEnvironment = "LLMGW_TASK11_PERSISTENCE_CHILD"

// isolatedHTTPResult reports a client.Do result without testing from its worker.
type isolatedHTTPResult struct {
	response *http.Response // response is non-nil once downstream headers arrive.
	err      error          // err reports request construction or transport failure.
}

// runIsolatedIntegrationTest confines permit-retaining scenarios to a fresh SDK lifecycle.
func runIsolatedIntegrationTest(t *testing.T, environment string, child func(*testing.T)) {
	t.Helper()
	if os.Getenv(environment) == "1" {
		child(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=^"+t.Name()+"$",
		"-test.count=1",
	)
	command.Env = append(os.Environ(), environment+"=1")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("isolated integration cleanup timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("isolated integration failed: %v; output=%s", err, safeBodySummary(output))
	}
}

// runLiveCancellationChild cancels while each real upstream executor remains blocked.
func runLiveCancellationChild(t *testing.T) {
	defer closeRetainedPermitHarness(t)
	created := testHarness.createKey(t, "protocol-cancel")
	t.Run("before first client chunk", func(t *testing.T) {
		cancelBeforeFirstClientChunk(t, created)
	})
	t.Run("after first client chunk", func(t *testing.T) {
		cancelAfterFirstClientChunk(t, created)
	})
	t.Run("delayed replay mutation is detected", func(t *testing.T) {
		assertDelayedCancellationReplayDetected(t, created)
	})
	testHarness.assertSecretsAbsent(t, created.Plaintext, "fixture-prompt")
}

// closeRetainedPermitHarness runs every cleanup stage and acknowledges only the bounded drain error.
func closeRetainedPermitHarness(t *testing.T) {
	t.Helper()
	root := testHarness.root
	result := testHarness.closeComponents()
	if err := validateRetainedPermitCleanup(result); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatal("isolated retained-permit teardown left temporary resources")
	}
	testHarness.closeErr = nil
}

// validateRetainedPermitCleanup accepts only the exact service drain chain and nil peers.
func validateRetainedPermitCleanup(result harnessCloseResult) error {
	if result.serviceErr != nil && !isExpectedRetainedPermitDrain(result.serviceErr) {
		return fmt.Errorf("retained-permit service teardown was not the expected drain: %v", result.serviceErr)
	}
	checks := []struct {
		name string
		err  error
	}{
		{"log audit", result.logErr},
		{"local resources", result.localErr},
		{"container", result.containerErr},
		{"temporary files", result.tempErr},
	}
	for _, check := range checks {
		if check.err != nil {
			return fmt.Errorf("retained-permit %s cleanup failed: %w", check.name, check.err)
		}
	}
	return nil
}

// isExpectedRetainedPermitDrain matches the exact run -> usage drain -> deadline chain.
func isExpectedRetainedPermitDrain(err error) bool {
	const exact = "run embedded proxy:\ndrain embedded CLIProxyAPI usage groups:\ncontext deadline exceeded"
	return err != nil && err.Error() == exact && errors.Is(err, context.DeadlineExceeded)
}

// cancelBeforeFirstClientChunk cancels while upstream is blocked before response headers.
func cancelBeforeFirstClientChunk(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	rowsBefore := testHarness.countRequests(t, created.Key.ProjectID, governance.OperationGeneration, "")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	response := streamingUsageResponse(2, 1)
	response.Started = started
	response.Release = release
	testHarness.Upstream.Enqueue(response)
	before := testHarness.Upstream.RequestCount()

	ctx, cancel := context.WithCancel(context.Background())
	request := newStreamingRequest(t, ctx, created.Plaintext)
	result := make(chan isolatedHTTPResult, 1)
	go func() {
		response, err := testHarness.client.Do(request)
		result <- isolatedHTTPResult{response: response, err: err}
	}()
	awaitSignal(t, started, "pre-chunk cancellation never reached upstream")
	cancel()
	close(release)

	outcome := awaitIsolatedHTTPResult(t, result)
	if outcome.response != nil {
		_ = outcome.response.Body.Close()
	}
	if outcome.err == nil {
		t.Fatal("pre-chunk canceled request returned a response")
	}
	assertCanceledRequestQuiescent(t, created.Key.ProjectID, rowsBefore, before)
}

// cancelAfterFirstClientChunk cancels after one frame while upstream is blocked before terminal.
func cancelAfterFirstClientChunk(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	rowsBefore := testHarness.countRequests(t, created.Key.ProjectID, governance.OperationGeneration, "")
	flushed := make(chan struct{}, 1)
	release := make(chan struct{})
	testHarness.Upstream.Enqueue(StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks: []StubChunk{{
			Body:    `data: {"id":"chatcmpl-live-cancel","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n",
			Flushed: flushed,
			Release: release,
		}},
	})
	before := testHarness.Upstream.RequestCount()
	ctx, cancel := context.WithCancel(context.Background())
	request := newStreamingRequest(t, ctx, created.Plaintext)
	response, err := testHarness.client.Do(request)
	if err != nil {
		close(release)
		t.Fatal("start post-chunk cancellation request failed")
	}
	awaitSignal(t, flushed, "post-chunk cancellation never flushed upstream")
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || !bytes.Contains([]byte(line), []byte("chatcmpl-live-cancel")) {
		close(release)
		t.Fatal("post-chunk cancellation did not read the first client frame")
	}
	cancel()
	_ = response.Body.Close()
	close(release)
	assertCanceledRequestQuiescent(t, created.Key.ProjectID, rowsBefore, before)
}

// assertDelayedCancellationReplayDetected proves the quiescence check outlives client return.
func assertDelayedCancellationReplayDetected(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	started, delayedStarted := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	first := streamingUsageResponse(1, 1)
	first.Started, first.Release = started, release
	testHarness.Upstream.Enqueue(first, StubResponse{
		Status:  http.StatusNoContent,
		Started: delayedStarted,
	})
	rowsBefore := testHarness.countRequests(t, created.Key.ProjectID, governance.OperationGeneration, "")
	upstreamBefore := testHarness.Upstream.RequestCount()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan isolatedHTTPResult, 1)
	request := newStreamingRequest(t, ctx, created.Plaintext)
	go func() {
		response, err := testHarness.client.Do(request)
		result <- isolatedHTTPResult{response: response, err: err}
	}()
	awaitSignal(t, started, "delayed-replay mutation never reached upstream")

	tx, err := testHarness.db.Begin(context.Background())
	if err != nil {
		t.Fatal("begin handler-completion lock failed")
	}
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(
		context.Background(),
		`SELECT id FROM request_event WHERE project_id = $1 AND state = 'in_flight'
		 ORDER BY requested_at DESC LIMIT 1 FOR UPDATE`,
		created.Key.ProjectID,
	).Scan(new(string)); err != nil {
		t.Fatal("lock cancellation request completion failed")
	}
	cancel()
	releaseOnce.Do(func() { close(release) })
	outcome := awaitIsolatedHTTPResult(t, result)
	if outcome.response != nil {
		_ = outcome.response.Body.Close()
	}
	if outcome.err == nil {
		t.Fatal("delayed-replay mutation did not cancel the client")
	}
	if got := testHarness.Upstream.RequestCount() - upstreamBefore; got != 1 {
		t.Fatalf("legacy immediate replay snapshot = %d, want 1", got)
	}

	delayed := make(chan error, 1)
	go func() {
		response, err := testHarness.client.Get(testHarness.Upstream.URL())
		if response != nil {
			_ = response.Body.Close()
		}
		delayed <- err
	}()
	awaitSignal(t, delayedStarted, "delayed second upstream attempt did not start")
	if completed := testHarness.countCompletedRequests(t, created.Key.ProjectID); completed != rowsBefore {
		t.Fatal("handler completed before delayed upstream mutation")
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal("release handler-completion lock failed")
	}
	if err := <-delayed; err != nil {
		t.Fatal("delayed upstream mutation failed")
	}
	if err := awaitCanceledRequestQuiescence(
		created.Key.ProjectID,
		rowsBefore,
		upstreamBefore,
	); err == nil {
		t.Fatal("quiescent replay assertion accepted a delayed second upstream attempt")
	}
}

// newStreamingRequest creates one cancellable real gateway request.
func newStreamingRequest(t *testing.T, ctx context.Context, plaintext string) *http.Request {
	t.Helper()
	body := `{"model":"test-model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"fixture-prompt"}]}`
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		testHarness.BaseURL+"/v1/chat/completions",
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatal("create cancellation request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+plaintext)
	return request
}

// awaitIsolatedHTTPResult bounds every worker join.
func awaitIsolatedHTTPResult(t *testing.T, result <-chan isolatedHTTPResult) isolatedHTTPResult {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request worker did not return")
		return isolatedHTTPResult{}
	}
}

// assertCanceledRequestQuiescent waits past handler completion before checking replay stability.
func assertCanceledRequestQuiescent(
	t *testing.T,
	projectID int64,
	rowsBefore int64,
	upstreamBefore int,
) {
	t.Helper()
	if err := awaitCanceledRequestQuiescence(projectID, rowsBefore, upstreamBefore); err != nil {
		t.Fatal(err)
	}
}

// awaitCanceledRequestQuiescence observes durable completion, upstream idle, and a stable count.
func awaitCanceledRequestQuiescence(projectID int64, rowsBefore int64, upstreamBefore int) error {
	const query = `
SELECT count(*), count(*) FILTER (WHERE state <> 'completed')
FROM request_event WHERE project_id = $1`
	wantRows := rowsBefore + 1
	deadline := time.Now().Add(5 * time.Second)
	for {
		var rows, incomplete int64
		err := testHarness.db.QueryRow(context.Background(), query, projectID).Scan(&rows, &incomplete)
		if err == nil && rows == wantRows && incomplete == 0 &&
			testHarness.Upstream.ActiveRequestCount() == 0 {
			break
		}
		if err == nil && rows > wantRows {
			return fmt.Errorf("canceled request rows = %d, want %d", rows, wantRows)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("canceled request handler did not become quiescent: %v", err)
		}
		<-time.After(10 * time.Millisecond)
	}

	stableUntil := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(stableUntil) {
		var rows, incomplete int64
		err := testHarness.db.QueryRow(context.Background(), query, projectID).Scan(&rows, &incomplete)
		attempts := testHarness.Upstream.RequestCount() - upstreamBefore
		if err != nil || rows != wantRows || incomplete != 0 ||
			testHarness.Upstream.ActiveRequestCount() != 0 || attempts != 1 {
			return fmt.Errorf(
				"canceled request changed after handler completion: rows=%d incomplete=%d attempts=%d active=%d err=%v",
				rows, incomplete, attempts, testHarness.Upstream.ActiveRequestCount(), err,
			)
		}
		<-time.After(10 * time.Millisecond)
	}
	return nil
}

// TestRetainedPermitCleanupRejectsConcurrentFailures catches joined-error substring acceptance.
func TestRetainedPermitCleanupRejectsConcurrentFailures(t *testing.T) {
	drain := retainedPermitDrainFixture()
	tests := []struct {
		name   string
		mutate func(*Harness)
	}{
		{"registered log leak", func(h *Harness) {
			h.logs = &lockedBuffer{}
			h.registerSecrets("cleanup-log-mutation-secret")
			_, _ = h.logs.Write([]byte("cleanup-log-mutation-secret"))
		}},
		{"resource cleanup failure", func(h *Harness) {
			h.resourceCleanupFailure = errors.New("cleanup-resource-mutation")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := &Harness{}
			harness.stopOnce.Do(func() { harness.stopErr = drain })
			test.mutate(harness)
			result := harness.closeComponents()
			if !strings.Contains(result.joined().Error(), "drain embedded CLIProxyAPI usage groups") {
				t.Fatal("legacy substring acceptance mutation was not exercised")
			}
			if err := validateRetainedPermitCleanup(result); err == nil {
				t.Fatal("retained-permit cleanup swallowed a concurrent cleanup failure")
			}
		})
	}
}

// retainedPermitDrainFixture mirrors the exact unwrap chain produced by Service.Run.
func retainedPermitDrainFixture() error {
	drain := fmt.Errorf("drain embedded CLIProxyAPI usage groups:\n%w", context.DeadlineExceeded)
	return fmt.Errorf("run embedded proxy:\n%w", drain)
}

// TestDatabaseUnavailableFailsBeforeUpstream catches retries of infrastructure 503 responses.
func TestDatabaseUnavailableFailsBeforeUpstream(t *testing.T) {
	created := testHarness.createKey(t, "failure-database")
	if _, err := testHarness.db.Exec(
		context.Background(),
		`ALTER TABLE client_key RENAME TO task11_client_key_unavailable`,
	); err != nil {
		t.Fatal("hide client_key table failed")
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			_, _ = testHarness.db.Exec(
				context.Background(),
				`ALTER TABLE task11_client_key_unavailable RENAME TO client_key`,
			)
		}
	})

	before := testHarness.Upstream.RequestCount()
	status, body := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusServiceUnavailable ||
		!bytes.Contains(body, []byte(`"type":"service_unavailable"`)) {
		t.Fatalf("database failure status/body = %d/%s", status, safeBodySummary(body))
	}
	if got := testHarness.Upstream.RequestCount(); got != before {
		t.Fatalf("database failure contacted upstream %d times", got-before)
	}
	if _, err := testHarness.db.Exec(
		context.Background(),
		`ALTER TABLE task11_client_key_unavailable RENAME TO client_key`,
	); err != nil {
		t.Fatal("restore client_key table failed")
	}
	restored = true
	testHarness.Upstream.Enqueue(jsonUsageResponse(2, 1))
	status, _ = authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("service after database restore = %d, want 200", status)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 1)
}

// TestUsagePersistenceFailureAndGrantRestore isolates the intentional failed-permit state.
func TestUsagePersistenceFailureAndGrantRestore(t *testing.T) {
	runIsolatedIntegrationTest(t, persistenceChildEnvironment, runUsagePersistenceFailureChild)
}

func runUsagePersistenceFailureChild(t *testing.T) {
	defer closeRetainedPermitHarness(t)
	created := testHarness.createKey(t, "failure-persistence")
	testHarness.denyUsageInserts(t)
	defer testHarness.cleanupUsageInsertRole(t)
	testHarness.assertUsageInsertPrivilege(t, false)
	testHarness.Upstream.Enqueue(jsonUsageResponse(3, 1))
	status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("client response during usage persistence failure = %d, want 200", status)
	}
	awaitCompletedRequestWithoutUsage(t, created.Key.ProjectID)
	result, err := testHarness.Store.ReconcileAccounting(
		context.Background(),
		time.Now().Add(time.Minute),
		0,
		6*time.Hour,
	)
	if err != nil || result.Unknown != 1 {
		t.Fatalf("reconcile missing usage = %#v, %v", result, err)
	}
	testHarness.setBudget(t, created, governance.DimensionTokens, 100, governance.ActionBlock)
	status, _ = authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusPaymentRequired {
		t.Fatalf("accounting_unknown token status = %d, want 402", status)
	}

	testHarness.allowUsageInserts(t)
	testHarness.assertUsageInsertPrivilege(t, true)
	recovered := testHarness.createKey(t, "failure-persistence-restored")
	testHarness.Upstream.Enqueue(jsonUsageResponse(2, 1))
	status, _ = authenticatedGeneration(t, recovered.Plaintext, "test-model")
	if status != http.StatusOK {
		t.Fatalf("client response after INSERT restore = %d, want 200", status)
	}
	awaitUsageAttempts(t, recovered.Key.ProjectID, 1)
	testHarness.assertSecretsAbsent(t, created.Plaintext, recovered.Plaintext)
}

// TestHostileConfigMutationIsIgnored catches re-enabling native access or management reload.
func TestHostileConfigMutationIsIgnored(t *testing.T) {
	created := testHarness.createKey(t, "failure-hostile-config")
	original, err := os.ReadFile(testHarness.ConfigPath)
	if err != nil {
		t.Fatal("read integration configuration failed")
	}
	hostile := append([]byte("api-keys: [native-bypass]\n"), original...)
	hostile = bytes.Replace(
		hostile,
		[]byte("remote-management:\n  allow-remote: false\n  secret-key: \"\"\n  disable-control-panel: true"),
		[]byte("remote-management:\n  allow-remote: false\n  secret-key: attempted-management-key\n  disable-control-panel: false"),
		1,
	)
	if err := os.WriteFile(testHarness.ConfigPath, hostile, 0o600); err != nil {
		t.Fatal("write hostile integration configuration failed")
	}
	t.Cleanup(func() { _ = os.WriteFile(testHarness.ConfigPath, original, 0o600) })
	time.Sleep(200 * time.Millisecond)

	nativeStatus, _ := gatewayRequest(
		t,
		http.MethodGet,
		"/v1/models",
		nil,
		requestHeaders{authorization: "Bearer native-bypass"},
	)
	if nativeStatus != http.StatusUnauthorized {
		t.Fatalf("native bypass status = %d, want 401", nativeStatus)
	}
	validStatus, _ := gatewayRequest(
		t,
		http.MethodGet,
		"/v1/models",
		nil,
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if validStatus != http.StatusOK {
		t.Fatalf("LLMGW key after hostile config = %d, want 200", validStatus)
	}
	for _, path := range deniedRoutes {
		status, _ := gatewayRequest(
			t,
			http.MethodGet,
			path,
			nil,
			requestHeaders{authorization: "Bearer " + created.Plaintext},
		)
		if status != http.StatusNotFound {
			t.Fatalf("hostile config exposed %s with status %d", path, status)
		}
	}
	if err := os.WriteFile(testHarness.ConfigPath, original, 0o600); err != nil {
		t.Fatal("restore integration configuration failed")
	}
	status, _ := gatewayRequest(
		t,
		http.MethodGet,
		"/v1/models",
		nil,
		requestHeaders{authorization: "Bearer " + created.Plaintext},
	)
	if status != http.StatusOK {
		t.Fatalf("service after config restore = %d, want 200", status)
	}
	testHarness.assertSecretsAbsent(t, created.Plaintext, "native-bypass", "attempted-management-key")
}

func awaitCompletedRequestWithoutUsage(t *testing.T, projectID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var completed, attempts int64
		err := testHarness.db.QueryRow(
			context.Background(),
			`SELECT
			   count(*) FILTER (WHERE state = 'completed'),
			   (SELECT count(*) FROM usage_attempt a
			    JOIN request_event r2 ON r2.id = a.request_id
			    WHERE r2.project_id = $1)
			 FROM request_event WHERE project_id = $1`,
			projectID,
		).Scan(&completed, &attempts)
		if err == nil && completed == 1 && attempts == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("completed client response did not retain missing usage")
}

// denyUsageInserts transfers ownership, then revokes the service role's real INSERT privilege.
func (h *Harness) denyUsageInserts(t *testing.T) {
	t.Helper()
	const statement = `
CREATE ROLE task11_usage_owner NOLOGIN;
ALTER TABLE usage_attempt OWNER TO task11_usage_owner;
GRANT SELECT, UPDATE, DELETE ON TABLE usage_attempt TO llmgw_service;
REVOKE INSERT ON TABLE usage_attempt FROM PUBLIC;
REVOKE INSERT ON TABLE usage_attempt FROM llmgw_service`
	if _, err := h.db.Exec(context.Background(), statement); err != nil {
		t.Fatal("deny integration usage inserts failed")
	}
}

// allowUsageInserts grants INSERT back while the service remains a non-owner.
func (h *Harness) allowUsageInserts(t *testing.T) {
	t.Helper()
	if _, err := h.db.Exec(
		context.Background(),
		`GRANT INSERT ON TABLE usage_attempt TO llmgw_service`,
	); err != nil {
		t.Fatal("restore integration usage inserts failed")
	}
}

// assertUsageInsertPrivilege proves REVOKE and GRANT changed the live service role.
func (h *Harness) assertUsageInsertPrivilege(t *testing.T, want bool) {
	t.Helper()
	const query = `
SELECT pg_get_userbyid(c.relowner),
       has_table_privilege('llmgw_service', 'public.usage_attempt', 'INSERT')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relname = 'usage_attempt'`
	var owner string
	var got bool
	if err := h.db.QueryRow(context.Background(), query).Scan(&owner, &got); err != nil {
		t.Fatal("inspect integration INSERT privilege failed")
	}
	if owner != "task11_usage_owner" || got != want {
		t.Fatalf("service usage INSERT privilege = owner:%s granted:%t, want non-owner/%t",
			owner, got, want)
	}
}

// cleanupUsageInsertRole restores ownership even when the isolated assertion fails.
func (h *Harness) cleanupUsageInsertRole(t *testing.T) {
	t.Helper()
	const statement = `
ALTER TABLE usage_attempt OWNER TO llmgw;
DROP ROLE task11_usage_owner`
	if _, err := h.db.Exec(context.Background(), statement); err != nil {
		t.Error("cleanup integration usage role failed")
	}
}
