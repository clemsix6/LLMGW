package claudemax

import (
	"strings"
	"testing"
)

// recordingSink captures the relayed bytes and counts flushes so a test can assert the relay is
// byte-exact and flushed per event.
type recordingSink struct {
	buf strings.Builder // buf accumulates everything the relay writes.

	flushes int // flushes counts how many times the relay flushed.
}

// Write appends to the buffer.
func (s *recordingSink) Write(p []byte) (int, error) {
	return s.buf.Write(p)
}

// Flush records a flush.
func (s *recordingSink) Flush() {
	s.flushes++
}

// cannedStream is a representative Anthropic SSE response with two message_delta events so the
// test can prove the latest output_tokens wins (rather than being summed).
const cannedStream = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n" +
	"\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n" +
	"\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n" +
	"\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":21}}\n" +
	"\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n" +
	"\n"

// TestRelayStreamAccumulatesUsage feeds a canned SSE stream through relayStream and asserts the
// input comes from message_start and the output is the latest message_delta value (not the sum).
func TestRelayStreamAccumulatesUsage(t *testing.T) {
	sink := &recordingSink{}

	got, err := relayStream(strings.NewReader(cannedStream), sink)
	if err != nil {
		t.Fatalf("relayStream: %v", err)
	}

	if got.InputTokens != 12 {
		t.Errorf("InputTokens = %d, want 12 (from message_start)", got.InputTokens)
	}
	if got.OutputTokens != 21 {
		t.Errorf("OutputTokens = %d, want 21 (latest message_delta, not the sum)", got.OutputTokens)
	}
}

// TestRelayStreamRelaysBytesVerbatim asserts the relay writes the upstream bytes unchanged (no
// buffering or mutation) and flushes at least once per event.
func TestRelayStreamRelaysBytesVerbatim(t *testing.T) {
	sink := &recordingSink{}

	if _, err := relayStream(strings.NewReader(cannedStream), sink); err != nil {
		t.Fatalf("relayStream: %v", err)
	}

	if sink.buf.String() != cannedStream {
		t.Errorf("relayed bytes differ from upstream:\ngot:  %q\nwant: %q", sink.buf.String(), cannedStream)
	}

	const events = 5 // message_start, content_block_delta, two message_delta, message_stop
	if sink.flushes < events {
		t.Errorf("flushes = %d, want >= %d (one per SSE event)", sink.flushes, events)
	}
}

// TestSSEDataExtraction checks the data-line parser handles the "data:" prefix with and without
// a leading space and ignores non-data lines.
func TestSSEDataExtraction(t *testing.T) {
	cases := []struct {
		line     string
		wantData string
		wantOK   bool
	}{
		{"data: {\"a\":1}\n", `{"a":1}`, true},
		{"data:{\"a\":1}\n", `{"a":1}`, true},
		{"event: message_stop\n", "", false},
		{"\n", "", false},
	}

	for _, tc := range cases {
		data, ok := sseData([]byte(tc.line))
		if ok != tc.wantOK || string(data) != tc.wantData {
			t.Errorf("sseData(%q) = (%q, %v), want (%q, %v)", tc.line, data, ok, tc.wantData, tc.wantOK)
		}
	}
}
