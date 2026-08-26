package cliproxy

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// providerErrorBody is the fixture standing in for a provider error payload
// that quotes caller content back inside its own message.
const providerErrorBody = `{"error":{"type":"invalid_request_error","message":"secret-caller-content"}}`

// TestUpstreamFailureLogsDropTheProviderBody proves the embedded SDK's upstream
// failure warning reaches the log without the provider response body it carries.
// Without this, every failed generation writes provider text — which may quote
// the caller's own request — into the gateway's logs.
func TestUpstreamFailureLogsDropTheProviderBody(t *testing.T) {
	logger, captured := redactingLogger()

	logger.Warnf(
		"upstream execution failed: provider=%s model=%s auth=%s duration=%s err=%s",
		"claude", "claude-opus-5", "api_key=abc...xyz", "1ms", providerErrorBody,
	)

	written := captured.String()
	if strings.Contains(written, "secret-caller-content") {
		t.Fatal("upstream failure log retained the provider error body")
	}
	if !strings.Contains(written, "provider=claude") ||
		!strings.Contains(written, "model=claude-opus-5") {
		t.Fatalf("upstream failure log lost its diagnosable fields: %s", written)
	}
}

// TestUnrelatedWarningsSurviveTheRedaction proves the hook rewrites only the
// warning it targets. Without this, a redaction meant for one SDK message could
// silently truncate every other warning the gateway emits.
func TestUnrelatedWarningsSurviveTheRedaction(t *testing.T) {
	logger, captured := redactingLogger()

	logger.Warnf("credential refresh failed: err=%s", providerErrorBody)

	if !strings.Contains(captured.String(), "secret-caller-content") {
		t.Fatalf("unrelated warning was rewritten: %s", captured.String())
	}
}

// redactingLogger builds an isolated logger carrying the redaction hook, so the
// assertions never depend on the process-wide installation order.
func redactingLogger() (*log.Logger, *bytes.Buffer) {
	captured := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(captured)
	logger.AddHook(upstreamFailureRedaction{})
	return logger, captured
}
