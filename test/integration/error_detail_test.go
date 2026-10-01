package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

// storedError is the error text recorded for one failed request.
type storedError struct {
	Type    *string // Type is usage_attempt.upstream_error_type.
	Message *string // Message is usage_attempt.upstream_error_message.
}

// TestUpstream4xxErrorTextRecorded proves a standard upstream error JSON is
// stored as its type and message, and nothing else from the body.
func TestUpstream4xxErrorTextRecorded(t *testing.T) {
	created := testHarness.createKey(t, "error-text-json")
	testHarness.Upstream.Enqueue(StubResponse{
		Status:  http.StatusBadRequest,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{"error":{"type":"invalid_request_error","message":"tool_choice is not supported",` +
			`"param":"integration-param-secret"}}`,
	})

	status, _ := gatewayRequest(t, http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(chatFixture), requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	got := awaitStoredError(t, created.Key.ProjectID)
	if got.Type == nil || *got.Type != "invalid_request_error" ||
		got.Message == nil || *got.Message != "tool_choice is not supported" {
		t.Fatalf("attempt error = %v / %v", got.Type, got.Message)
	}
	testHarness.assertSecretsAbsent(t, created.Plaintext, "integration-param-secret")
}

// TestUpstream4xxNonJSONBodyStoresNothing proves a body that is not the
// standard error JSON leaves the error columns empty.
func TestUpstream4xxNonJSONBodyStoresNothing(t *testing.T) {
	created := testHarness.createKey(t, "error-text-plain")
	testHarness.Upstream.Enqueue(StubResponse{
		Status:  http.StatusBadRequest,
		Headers: http.Header{"Content-Type": []string{"text/plain"}},
		Body:    "plain failure carrying integration-plain-secret",
	})

	status, _ := gatewayRequest(t, http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(chatFixture), requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	got := awaitStoredError(t, created.Key.ProjectID)
	if got.Type != nil || got.Message != nil {
		t.Fatalf("stored error for non-JSON body = %v / %v", got.Type, got.Message)
	}
	testHarness.assertSecretsAbsent(t, created.Plaintext, "integration-plain-secret")
}

// awaitStoredError polls until the request is complete and its failed attempt
// is durable, then returns the recorded error columns.
func awaitStoredError(t *testing.T, projectID int64) storedError {
	t.Helper()
	const query = `
SELECT a.upstream_error_type, a.upstream_error_message
FROM usage_attempt a
JOIN request_event r ON r.id = a.request_id
WHERE r.project_id = $1 AND r.state = 'completed' AND a.failed`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got storedError
		err := testHarness.db.QueryRow(context.Background(), query, projectID).Scan(
			&got.Type, &got.Message,
		)
		if err == nil {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("failed attempt with a completed request did not become durable")
	return storedError{}
}
