package cliproxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// toolUseResponse is one complete Anthropic response carrying a prefixed
// tool call.
const toolUseResponse = `{"id":"msg_1","type":"message","role":"assistant",` +
	`"content":[{"type":"tool_use","id":"tu_1","name":"mcp__llmgw__search_web","input":{"q":"go"}}],` +
	`"stop_reason":"tool_use"}`

// toolUseEvent is one complete server-sent event announcing a prefixed tool
// call, delimiter included.
const toolUseEvent = "event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,` +
	`"content_block":{"type":"tool_use","id":"tu_1","name":"mcp__llmgw__search_web","input":{}}}` +
	"\n\n"

// wrappedWriter installs a tool-prefix writer over the real gin response
// writer, which is the delegate it wraps in production.
func wrappedWriter(t *testing.T) (*gin.Context, *httptest.ResponseRecorder, *toolPrefixWriter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := newToolPrefixWriter(c.Writer)
	c.Writer = writer
	return c, recorder, writer
}

// TestBufferedModeRewritesTheBodyAndItsLength proves a non-streamed response
// reaches the client without the prefix, and with the length it actually has:
// the rewrite shortens the body, so a length committed before it would lie.
func TestBufferedModeRewritesTheBodyAndItsLength(t *testing.T) {
	for _, write := range []struct {
		name string
		emit func(c *gin.Context)
	}{
		{"Write", func(c *gin.Context) { _, _ = c.Writer.Write([]byte(toolUseResponse)) }},
		{"WriteString", func(c *gin.Context) { _, _ = c.Writer.WriteString(toolUseResponse) }},
	} {
		t.Run(write.name, func(t *testing.T) {
			c, recorder, writer := wrappedWriter(t)
			c.Header("Content-Type", "application/json")
			write.emit(c)

			if recorder.Body.Len() != 0 {
				t.Fatalf("buffered mode wrote %q before finalization", recorder.Body.String())
			}
			if !c.Writer.Written() {
				t.Fatal("Written() = false with a held body, want true")
			}
			writer.finalize()

			body := recorder.Body.String()
			if !strings.Contains(body, `"name":"search_web"`) || strings.Contains(body, "mcp__llmgw__search_web") {
				t.Fatalf("finalized body = %s, want the prefix stripped", body)
			}
			length := recorder.Result().Header.Get("Content-Length")
			if length != strconv.Itoa(len(body)) {
				t.Fatalf("Content-Length = %q, want %d", length, len(body))
			}
		})
	}
}

// TestBufferedModeKeepsLeadingKeepAliveFrames proves a configured
// non-streaming keep-alive does not make the payload read as malformed: the
// newlines it emits are kept and the JSON behind them is still rewritten.
func TestBufferedModeKeepsLeadingKeepAliveFrames(t *testing.T) {
	c, recorder, writer := wrappedWriter(t)
	c.Header("Content-Type", "application/json")
	_, _ = c.Writer.Write([]byte("\n"))
	_, _ = c.Writer.Write([]byte("\n"))
	_, _ = c.Writer.Write([]byte(toolUseResponse))
	writer.finalize()

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "\n\n") {
		t.Fatalf("body = %q, want the keep-alive frames preserved", body)
	}
	if !strings.Contains(body, `"name":"search_web"`) || strings.Contains(body, "mcp__llmgw__search_web") {
		t.Fatalf("body = %s, want the prefix stripped behind the keep-alive", body)
	}
}

// TestStreamingModeRewritesEachCompleteEvent proves an event forwarded whole
// and one split across writes both reach the client as a single coherent
// stripped event, and that nothing leaves before its delimiter arrives.
func TestStreamingModeRewritesEachCompleteEvent(t *testing.T) {
	c, recorder, writer := wrappedWriter(t)
	c.Header("Content-Type", "text/event-stream")

	_, _ = c.Writer.Write([]byte(toolUseEvent))
	whole := recorder.Body.String()
	if !strings.Contains(whole, `"name":"search_web"`) || strings.Contains(whole, "mcp__llmgw__search_web") {
		t.Fatalf("whole event = %q, want the prefix stripped", whole)
	}

	recorder.Body.Reset()
	for _, chunk := range splitThirds(toolUseEvent) {
		_, _ = c.Writer.Write([]byte(chunk))
	}
	writer.finalize()

	split := recorder.Body.String()
	if split != whole {
		t.Fatalf("split event = %q, want the same coherent event as %q", split, whole)
	}
}

// TestStreamingModeHoldsIncompleteEvents proves a partial event stays buffered
// until its delimiter arrives, so no half event ever reaches the client.
func TestStreamingModeHoldsIncompleteEvents(t *testing.T) {
	c, recorder, writer := wrappedWriter(t)
	c.Header("Content-Type", "text/event-stream")

	partial := toolUseEvent[:len(toolUseEvent)-4]
	_, _ = c.Writer.Write([]byte(partial))
	if recorder.Body.Len() != 0 {
		t.Fatalf("undelimited event wrote %q, want nothing", recorder.Body.String())
	}

	// Finalization flushes the residue rather than truncating the response.
	writer.finalize()
	if recorder.Body.String() != partial {
		t.Fatalf("residual = %q, want %q", recorder.Body.String(), partial)
	}
}

// TestStreamingModeSurvivesAMidStreamStatus proves the SDK's terminal-error
// path — c.Status after the headers are on the wire — neither changes the
// status the client already received nor flips the wrapper out of streaming.
func TestStreamingModeSurvivesAMidStreamStatus(t *testing.T) {
	c, recorder, writer := wrappedWriter(t)
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.Write([]byte(toolUseEvent))
	recorder.Body.Reset()

	c.Status(http.StatusInternalServerError)
	_, _ = c.Writer.Write([]byte(toolUseEvent))
	writer.finalize()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d after a mid-stream error, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(recorder.Body.String(), `"name":"search_web"`) {
		t.Fatalf("post-error event = %q, want it still streamed and stripped", recorder.Body.String())
	}
}

// TestStreamingModeCommitsHeadersOnAFlush proves the SDK's empty-stream path —
// set the headers, flush, write nothing — reaches the client rather than
// hanging behind a wrapper waiting for a first write.
func TestStreamingModeCommitsHeadersOnAFlush(t *testing.T) {
	c, recorder, _ := wrappedWriter(t)
	c.Header("Content-Type", "text/event-stream")
	c.Writer.Flush()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d after a header-only flush, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Result().Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("committed Content-Type = %q, want text/event-stream", got)
	}
}

// TestStreamingModeForwardsAnUndelimitedFlood proves a stream that never
// delimits is forwarded rather than held, and that accumulation restarts
// cleanly behind it.
func TestStreamingModeForwardsAnUndelimitedFlood(t *testing.T) {
	c, recorder, writer := wrappedWriter(t)
	c.Header("Content-Type", "text/event-stream")

	flood := strings.Repeat("x", streamBufferLimit+1)
	_, _ = c.Writer.Write([]byte(flood))
	if recorder.Body.Len() != len(flood) {
		t.Fatalf("forwarded %d bytes, want the whole %d-byte flood", recorder.Body.Len(), len(flood))
	}

	recorder.Body.Reset()
	_, _ = c.Writer.Write([]byte(toolUseEvent))
	writer.finalize()
	if !strings.Contains(recorder.Body.String(), `"name":"search_web"`) {
		t.Fatalf("event after the flood = %q, want it stripped", recorder.Body.String())
	}
}

// TestNonOkResponsesPassThroughUnchanged proves an error body reaches the
// client verbatim: it carries no tool names, and rewriting it would replace
// upstream error reporting with ours.
func TestNonOkResponsesPassThroughUnchanged(t *testing.T) {
	c, recorder, writer := wrappedWriter(t)
	c.Status(http.StatusBadRequest)
	c.Header("Content-Type", "application/json")
	_, _ = c.Writer.Write([]byte(toolUseResponse))
	writer.finalize()

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if recorder.Body.String() != toolUseResponse {
		t.Fatalf("error body = %s, want it verbatim", recorder.Body.String())
	}
}

// splitThirds cuts one payload into three consecutive chunks.
func splitThirds(payload string) []string {
	first := len(payload) / 3
	second := 2 * len(payload) / 3
	return []string{payload[:first], payload[first:second], payload[second:]}
}
