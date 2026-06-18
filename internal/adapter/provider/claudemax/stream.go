package claudemax

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// handleStreamResponse maps a streaming upstream response: 200 relays the SSE while
// accumulating usage, 429 becomes a RateLimitError, any other status an UpstreamError. A
// non-200 error is returned before any byte reaches out, so the handler still owns the status.
func handleStreamResponse(resp *http.Response, out domain.StreamSink) (usage.Usage, error) {
	if resp.StatusCode == http.StatusOK {
		return relayStream(resp.Body, out)
	}
	return usage.Usage{}, streamStatusError(resp)
}

// streamStatusError reads the upstream body and maps a non-200 streaming response to a typed
// error (a rate limit carries the reset time when present).
func streamStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{ResetAt: parseResetAt(resp.Header, time.Now())}
	}
	return &UpstreamError{Status: resp.StatusCode, Body: string(body)}
}

// relayStream copies the upstream SSE stream to out unbuffered — flushing after each event so
// streaming latency is preserved — while accumulating usage from the message_start (input) and
// message_delta (output) events. It returns the usage gathered so far when the stream ends; a
// read error (including ctx cancellation on client disconnect) stops the relay promptly.
func relayStream(body io.Reader, out domain.StreamSink) (usage.Usage, error) {
	reader := bufio.NewReader(body)
	var acc usageAccumulator

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if writeErr := relayLine(line, out, &acc); writeErr != nil {
				return acc.usage(), writeErr
			}
		}
		if err != nil {
			out.Flush()
			if err == io.EOF {
				return acc.usage(), nil
			}
			return acc.usage(), fmt.Errorf("read upstream stream:\n%w", err)
		}
	}
}

// relayLine writes a single SSE line through to out, taps it for usage, and flushes at the end
// of an event (a blank line terminates an SSE event).
func relayLine(line []byte, out domain.StreamSink, acc *usageAccumulator) error {
	if _, err := out.Write(line); err != nil {
		return fmt.Errorf("write stream event:\n%w", err)
	}
	acc.observe(line)
	if isBlankLine(line) {
		out.Flush()
	}
	return nil
}

// isBlankLine reports whether a relayed line is an SSE event terminator (empty after trimming
// the trailing newline).
func isBlankLine(line []byte) bool {
	return len(bytes.TrimRight(line, "\r\n")) == 0
}

// usageAccumulator gathers token usage from the SSE events as they stream past.
type usageAccumulator struct {
	inputTokens int // inputTokens is taken from the message_start event.

	outputTokens int // outputTokens is the latest value seen on a message_delta event.
}

// usage returns the accumulated counts.
func (a usageAccumulator) usage() usage.Usage {
	return usage.Usage{InputTokens: a.inputTokens, OutputTokens: a.outputTokens}
}

// observe parses an SSE data line and updates the running usage: input tokens come from
// message_start, output tokens from the latest message_delta (Anthropic reports cumulative
// output tokens on each message_delta, so the last one wins).
func (a *usageAccumulator) observe(line []byte) {
	data, ok := sseData(line)
	if !ok {
		return
	}

	var event streamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return // tolerate keep-alive comments and partial / non-JSON data lines
	}

	switch event.Type {
	case "message_start":
		a.inputTokens = event.Message.Usage.InputTokens
	case "message_delta":
		a.outputTokens = event.Usage.OutputTokens
	}
}

// sseData extracts the JSON payload of an SSE "data:" line, reporting whether the line carried
// data.
func sseData(line []byte) ([]byte, bool) {
	const prefix = "data:"
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte(prefix)) {
		return nil, false
	}
	return bytes.TrimSpace(trimmed[len(prefix):]), true
}

// streamEvent is the slice of an Anthropic SSE event the gateway meters: message_start carries
// input tokens under "message.usage"; message_delta carries cumulative output tokens under
// "usage".
type streamEvent struct {
	Type string `json:"type"` // Type is the SSE event type (e.g. "message_start").

	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"` // InputTokens is the prompt tokens consumed.
		} `json:"usage"` // Usage is the message-level usage on message_start.
	} `json:"message"` // Message is present on the message_start event.

	Usage struct {
		OutputTokens int `json:"output_tokens"` // OutputTokens is the cumulative generated tokens.
	} `json:"usage"` // Usage is present on the message_delta event.
}
