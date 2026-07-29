package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// gatewayResult reports one asynchronous gateway request.
type gatewayResult struct {
	status int   // status is the downstream HTTP status.
	err    error // err reports request or response failure.
}

// TestZZActiveRequestDrainsBeforeServiceReturn verifies real SDK HTTP draining.
func TestZZActiveRequestDrainsBeforeServiceReturn(t *testing.T) {
	created := testHarness.createKey(t, "drain")
	requestDone, release := beginBlockedGatewayRequest(t, created)
	stopDone := beginBlockedServiceStop(t)

	close(release)
	assertGatewayResult(t, requestDone)
	if err := <-stopDone; err != nil {
		t.Fatalf("stop service after drain: %v", err)
	}
	if got := testHarness.countCompletedRequests(t, created.Key.ProjectID); got != 1 {
		t.Fatalf("completed requests after drain = %d, want 1", got)
	}
	if rows := usageAttempts(t, created.Key.ProjectID); len(rows) != 1 {
		t.Fatalf("usage attempts after service return = %d, want 1", len(rows))
	}
	testHarness.assertSecretsAbsent(t, created.Plaintext)
}

// beginBlockedGatewayRequest starts a request paused inside the real upstream.
func beginBlockedGatewayRequest(
	t *testing.T,
	created governance.CreatedKey,
) (<-chan gatewayResult, chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	response := defaultStubResponse()
	response.Started = started
	response.Release = release
	testHarness.Upstream.Enqueue(response)

	done := make(chan gatewayResult, 1)
	go func() {
		status, err := asynchronousGatewayRequest(created.Plaintext)
		done <- gatewayResult{status: status, err: err}
	}()
	awaitSignal(t, started, "upstream request did not start")
	return done, release
}

// beginBlockedServiceStop proves shutdown remains blocked while work is active.
func beginBlockedServiceStop(t *testing.T) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- testHarness.stopService()
	}()
	select {
	case err := <-done:
		t.Fatalf("service returned before active request drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := testHarness.db.Ping(context.Background()); err != nil {
		t.Fatalf("database unavailable during service drain: %v", err)
	}
	return done
}

// asynchronousGatewayRequest sends one generation request without testing calls.
func asynchronousGatewayRequest(plaintext string) (int, error) {
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		testHarness.BaseURL+"/v1/chat/completions",
		bytes.NewBufferString(chatFixture),
	)
	if err != nil {
		return 0, fmt.Errorf("create drain request:\n%w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+plaintext)
	response, err := testHarness.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("send drain request:\n%w", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return 0, fmt.Errorf("read drain response:\n%w", err)
	}
	return response.StatusCode, nil
}

// assertGatewayResult verifies the drained request completed successfully.
func assertGatewayResult(t *testing.T, done <-chan gatewayResult) {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("drained gateway request: %v", result.err)
		}
		if result.status != http.StatusOK {
			t.Fatalf("drained gateway status = %d, want 200", result.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drained gateway request did not finish")
	}
}

// awaitSignal waits for a bounded integration lifecycle signal.
func awaitSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
}
