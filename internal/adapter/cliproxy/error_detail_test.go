package cliproxy

import (
	"strings"
	"testing"
	"time"

	sdkusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const secretMarker = "sk-secret-marker"

// failedRecord builds a failed SDK record with the given status and body.
func failedRecord(status int, body string) sdkusage.Record {
	return sdkusage.Record{
		Failed:      true,
		Fail:        sdkusage.Failure{StatusCode: status, Body: body},
		RequestedAt: time.Now(),
	}
}

// TestUpstreamErrorKeepsOnlyTypeAndMessage proves the stored text comes only
// from the parsed error JSON, never from other fields or non-JSON bodies.
func TestUpstreamErrorKeepsOnlyTypeAndMessage(t *testing.T) {
	standard := `{"error":{"type":"invalid_request_error","message":"bad thinking block",` +
		`"echo":"` + secretMarker + `"},"headers":{"authorization":"` + secretMarker + `"}}`
	got := mapUsageRecord(failedRecord(400, standard), usageCorrelation{}, "a").UpstreamError
	if got == nil || got.Type != "invalid_request_error" || got.Message != "bad thinking block" {
		t.Fatalf("standard error = %#v", got)
	}
	if strings.Contains(got.Type+got.Message, secretMarker) {
		t.Fatal("secret leaked into stored error")
	}

	for name, record := range map[string]sdkusage.Record{
		"non-json body": failedRecord(400, "upstream said "+secretMarker),
		"server error":  failedRecord(500, standard),
		"not failed":    {Fail: sdkusage.Failure{StatusCode: 400, Body: standard}, RequestedAt: time.Now()},
	} {
		if mapped := mapUsageRecord(record, usageCorrelation{}, "a").UpstreamError; mapped != nil {
			t.Fatalf("%s stored %#v, want nothing", name, mapped)
		}
	}
}
