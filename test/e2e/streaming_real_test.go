package e2e

// This suite drives the FULL gateway (POST /v1/messages with stream:true) against the REAL
// Anthropic API: it boots the gateway + an ephemeral Postgres, seeds a Claude Max refresh token,
// forwards a tiny streaming request, and reads the relayed SSE incrementally. It proves the SSE
// relay is unbuffered (time-to-first-event is far smaller than the total stream time) and that
// usage accumulated from the stream is recorded. Gated on LLMGW_TEST_REFRESH_TOKEN; skips when
// absent.
//
// TestMain refreshes the single-use token once and writes the rotation back to .env, so this test
// triggers no refresh of its own. Re-source .env before each run:
//
//	set -a; . ./.env; set +a; go test ./test/e2e -run Streaming -v

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

const (
	// streamingProject is the X-Project the streaming suite tracks usage under.
	streamingProject = "e2e-stream"

	// streamingTag is the X-Tags bucket the streaming suite attributes usage to.
	streamingTag = "stream"

	// streamBody asks for a few sentences so the generation spans enough time to prove the relay
	// streams events as they arrive rather than buffering the whole response.
	streamBody = `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 256,
		"stream": true,
		"messages": [{"role": "user", "content": "Write a short paragraph of four or five sentences about the ocean."}]
	}`
)

// streamResult captures what the consumer observed while reading the SSE stream.
type streamResult struct {
	events int // events is the number of SSE "event:" lines received.

	sawStop bool // sawStop reports whether a terminal message_stop event arrived.

	contentType string // contentType is the response Content-Type header.

	timeToFirst time.Duration // timeToFirst is the delay from request start to the first event.

	total time.Duration // total is the delay from request start to the end of the stream.
}

// TestPassthroughRealStreaming forwards a tiny streaming request through the full gateway and
// asserts a real SSE relay: multiple events ending in message_stop, an unbuffered delivery
// (first event well before the stream end), and an attributed usage_event with output tokens.
func TestPassthroughRealStreaming(t *testing.T) {
	token := requireSharedToken(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startPassthroughHarness(t, ctx, token)

	result := streamWithRetry(t, ctx, harness)
	assertStreamPlausible(t, result)

	assertUsageRecorded(t, ctx, harness.DSN, streamingProject, streamingTag)
}

// streamWithRetry issues the streaming POST, retrying transient gateway 5xx / network errors and
// a stream that ends without message_stop. It fails fast on the gateway's own assertions (dead
// token, rate limit, other 4xx).
func streamWithRetry(t *testing.T, ctx context.Context, h *Harness) streamResult {
	t.Helper()

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if result, done := attemptStream(t, ctx, h, attempt, &backoff); done {
			return result
		}
	}

	t.Fatalf("exhausted %d attempts against the real API", maxAttempts)
	return streamResult{}
}

// attemptStream performs one streaming POST and reads the SSE to completion. It returns the
// result with done=true on a full stream, fails the test on a fatal status, or sleeps and
// returns done=false on a transient failure.
func attemptStream(t *testing.T, ctx context.Context, h *Harness, attempt int, backoff *time.Duration) (streamResult, bool) {
	t.Helper()

	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := h.Post(attemptCtx, "/v1/messages", []byte(streamBody), streamHeaders())
	if err != nil {
		retryTransient(t, attempt, backoff, "POST /v1/messages: "+err.Error())
		return streamResult{}, false
	}

	if resp.StatusCode != http.StatusOK {
		classifyFailure(t, resp.StatusCode, readBody(t, resp), attempt, backoff)
		return streamResult{}, false
	}

	result := readStream(resp, start)
	if !result.sawStop {
		retryTransient(t, attempt, backoff, "stream ended without message_stop")
		return streamResult{}, false
	}
	return result, true
}

// streamHeaders are the headers the streaming request carries.
func streamHeaders() map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"X-Project":    streamingProject,
		"X-Tags":       streamingTag,
	}
}

// readStream consumes the SSE response line by line, recording the event count, whether a
// terminal message_stop arrived, and the time to the first event versus the full stream. start
// is the instant the request was issued, so the timings include the round-trip to first byte.
func readStream(resp *http.Response, start time.Time) streamResult {
	defer resp.Body.Close()

	result := streamResult{contentType: resp.Header.Get("Content-Type")}
	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if eventType, ok := sseEventType(line); ok {
			if result.events == 0 {
				result.timeToFirst = time.Since(start)
			}
			result.events++
			if eventType == "message_stop" {
				result.sawStop = true
			}
		}
		if err != nil {
			break
		}
	}

	result.total = time.Since(start)
	return result
}

// sseEventType returns the type of an SSE "event:" line, reporting whether the line was one.
func sseEventType(line string) (string, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "event:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")), true
}

// assertStreamPlausible checks the response was a real, unbuffered SSE stream: an event-stream
// content type, at least two events ending in message_stop, and a first event that arrives well
// before the stream completes (proving events were relayed as they arrived, not all at once).
func assertStreamPlausible(t *testing.T, result streamResult) {
	t.Helper()

	if !strings.HasPrefix(result.contentType, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", result.contentType)
	}
	if result.events < 2 {
		t.Errorf("received %d SSE events, want >= 2", result.events)
	}
	if !result.sawStop {
		t.Error("never received a terminal message_stop event")
	}

	if result.timeToFirst*2 >= result.total {
		t.Errorf("time-to-first-event %v is not much smaller than total %v (relay looks buffered)",
			result.timeToFirst, result.total)
	}

	t.Logf("streaming 200: events=%d stop=%v content_type=%q time_to_first=%v total=%v",
		result.events, result.sawStop, result.contentType, result.timeToFirst, result.total)
}
