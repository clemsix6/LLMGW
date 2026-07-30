package integration

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// alertWaitTimeout bounds every wait on an event the request path already produced.
const alertWaitTimeout = 5 * time.Second

// The two aliases these tests drive. Each resolves to an upstream model no other
// file requests, so no other test can move the same credential entity in this
// process and make a per-credential count depend on file order.
const (
	rateLimitAlias  = "cooldown-other-model"
	rateLimitModel  = "codex-other-model"
	failingAlias    = "cooldown-no-reset-model"
	failingModel    = "codex-no-reset-model"
	failingStatus   = "502"
	rateLimitStatus = "429"
)

// TestUpstreamRateLimitAlertsTheCredential proves one upstream 429 reports the
// credential that hit it while the request still succeeds on another.
func TestUpstreamRateLimitAlertsTheCredential(t *testing.T) {
	created := testHarness.createKey(t, "alert-rate-limited")
	mark := testHarness.Alerts.Mark()
	testHarness.Upstream.Enqueue(codexRateLimit(), codexUsageResponse(rateLimitModel))

	if status, _ := codexGeneration(t, created.Plaintext, rateLimitAlias); status != http.StatusOK {
		t.Fatalf("rate-limited failover status = %d, want 200", status)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 2)

	event, found := testHarness.Alerts.Wait(
		mark,
		alert.KindCredentialRateLimited,
		[]alert.Field{{Name: "Model", Value: rateLimitModel}},
		alertWaitTimeout,
	)
	if !found {
		t.Fatal("upstream 429 produced no credential_rate_limited event")
	}
	if event.Severity != alert.SeverityWarning {
		t.Fatalf("credential_rate_limited severity = %q, want warning", event.Severity)
	}
	assertCredentialIdentity(t, event, rateLimitModel, rateLimitStatus)
}

// TestRepeatedUpstreamFailureAlertsOncePerCredential proves the engine reports an
// identical credential failure exactly once and still reports the outage itself.
//
// A repeated 5xx is the case the harness config permits to recur:
// transient-error-cooldown-seconds is -1, so a failing credential stays
// immediately eligible and every request really does reach the same two
// (credential, model) pairs again. A repeated 429 would instead be answered from
// the SDK's model cooldown without contacting upstream, which proves nothing
// about deduplication.
func TestRepeatedUpstreamFailureAlertsOncePerCredential(t *testing.T) {
	created := testHarness.createKey(t, "alert-failing")
	mark := testHarness.Alerts.Mark()
	driveFailingGenerations(t, created, 3)

	model := alert.Field{Name: "Model", Value: failingModel}
	event, found := testHarness.Alerts.Wait(
		mark, alert.KindCredentialFailing, []alert.Field{model}, alertWaitTimeout,
	)
	if !found {
		t.Fatal("repeated upstream 5xx produced no credential_failing event")
	}
	if event.Severity != alert.SeverityWarning {
		t.Fatalf("credential_failing severity = %q, want warning", event.Severity)
	}
	assertCredentialIdentity(t, event, failingModel, failingStatus)

	credential := alert.Field{Name: "Credential", Value: fieldValue(event, "Credential")}
	got := testHarness.Alerts.CountFor(
		mark, alert.KindCredentialFailing, []alert.Field{model, credential},
	)
	if got != 1 {
		t.Fatalf("credential_failing messages for one credential = %d, want 1", got)
	}
	assertGenerationOutage(t, mark)
}

// TestBlockedBudgetAlertsTheProject proves an exhausted calls budget reports the
// project the middleware admitted, alongside the limit that denied it.
func TestBlockedBudgetAlertsTheProject(t *testing.T) {
	created := testHarness.createKey(t, "alert-budget-blocked")
	testHarness.setBudget(t, created, governance.DimensionCalls, 1, governance.ActionBlock)
	mark := testHarness.Alerts.Mark()

	testHarness.Upstream.Enqueue(jsonUsageResponse(2, 1))
	if status, _ := authenticatedGeneration(t, created.Plaintext, "test-model"); status != http.StatusOK {
		t.Fatalf("pre-budget status = %d, want 200", status)
	}
	awaitUsageAttempts(t, created.Key.ProjectID, 1)
	status, _ := authenticatedGeneration(t, created.Plaintext, "test-model")
	if status != http.StatusPaymentRequired {
		t.Fatalf("exhausted budget status = %d, want 402", status)
	}

	event, found := testHarness.Alerts.Wait(
		mark,
		alert.KindBudgetBlocked,
		[]alert.Field{{Name: "Project", Value: created.Key.ProjectName}},
		alertWaitTimeout,
	)
	if !found {
		t.Fatal("exhausted calls budget produced no budget_blocked event")
	}
	if event.Severity != alert.SeverityCritical {
		t.Fatalf("budget_blocked severity = %q, want critical", event.Severity)
	}
	if fieldValue(event, "Dimension") == "" || fieldValue(event, "Window") == "" {
		t.Fatal("budget_blocked carries no dimension or window")
	}
}

// driveFailingGenerations issues count generations whose every attempt fails.
//
// max-retry-credentials is 2, so each request needs two scripted failures and
// the client receives the upstream status. Awaiting the usage rows gates the
// observation the alert rides on, and fails on the generation that consumed the
// wrong number of scripts rather than several tests later. It cannot undo the
// offset: StubUpstream has no drain, so an unconsumed response stays queued for
// whichever test runs next.
func driveFailingGenerations(t *testing.T, created governance.CreatedKey, count int) {
	t.Helper()
	for generation := 1; generation <= count; generation++ {
		testHarness.Upstream.Enqueue(codexServerError(), codexServerError())
		status, _ := codexGeneration(t, created.Plaintext, failingAlias)
		if status < http.StatusInternalServerError {
			t.Fatalf("failing generation %d status = %d, want 5xx", generation, status)
		}
		awaitUsageAttempts(t, created.Key.ProjectID, 2*generation)
	}
}

// assertGenerationOutage checks the critical event repeated failures raise.
//
// The consecutive-failure counter is a single global on the tracker that every
// generation the rest of the package drives moves, so only its presence — never
// its value, and never the ordinal of the generation that raised it — is
// assertable from this suite.
func assertGenerationOutage(t *testing.T, mark int) {
	t.Helper()
	event, found := testHarness.Alerts.Wait(
		mark, alert.KindGenerationFailures, nil, alertWaitTimeout,
	)
	if !found {
		t.Fatal("repeated upstream 5xx produced no generation_failures event")
	}
	if event.Severity != alert.SeverityCritical {
		t.Fatalf("generation_failures severity = %q, want critical", event.Severity)
	}
	if fieldValue(event, "Consecutive failures") == "" {
		t.Fatal("generation_failures carries no consecutive failure count")
	}
}

// assertCredentialIdentity checks the operator-facing identity of one credential
// event: the resolved upstream model, never the alias the client sent.
func assertCredentialIdentity(t *testing.T, event alert.Event, model string, status string) {
	t.Helper()
	if fieldValue(event, "Provider") == "" {
		t.Fatal("credential event names no provider")
	}
	if fieldValue(event, "Credential") == "" {
		t.Fatal("credential event names no credential")
	}
	if got := fieldValue(event, "Model"); got != model {
		t.Fatalf("credential event model = %q, want %q", got, model)
	}
	if got := fieldValue(event, "Status"); got != status {
		t.Fatalf("credential event status = %q, want %q", got, status)
	}
}

// fieldValue returns one event field's value, or the empty string when absent.
func fieldValue(event alert.Event, name string) string {
	for _, field := range event.Fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

// codexRateLimit returns a Codex 429 carrying no reset hint.
func codexRateLimit() StubResponse {
	return StubResponse{
		Status: http.StatusTooManyRequests,
		Headers: http.Header{
			"Content-Type":     []string{"application/json"},
			"X-Secret-Fixture": []string{upstreamHeaderSecret},
		},
		Body: `{"error":{"type":"usage_limit_reached","message":"` + upstreamFailureSecret + `"}}`,
	}
}

// codexServerError returns a Codex 5xx whose body is deliberately not a
// registered fixture secret: every attempt of a fully failing request answers
// with it, and the SDK surfaces it to the client.
func codexServerError() StubResponse {
	return StubResponse{
		Status:  http.StatusBadGateway,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    `{"error":{"type":"server_error","message":"alert-upstream-failure-fixture"}}`,
	}
}

// codexUsageResponse returns a successful Codex stream for one resolved model.
func codexUsageResponse(model string) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: `data: {"type":"response.completed","response":{"id":"resp-cooldown",` +
			`"object":"response","created_at":1,"status":"completed","model":"` + model +
			`","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
	}
}

// codexGeneration performs one Codex generation for the given alias.
func codexGeneration(t *testing.T, plaintext string, model string) (int, []byte) {
	t.Helper()
	body := `{"model":"` + model + `","input":"fixture-prompt"}`
	return gatewayRequest(t, http.MethodPost, "/v1/responses", bytes.NewBufferString(body),
		requestHeaders{authorization: "Bearer " + plaintext})
}
