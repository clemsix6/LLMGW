package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// completionChunk is a streaming chat.completion.chunk event emitted to the client.
type completionChunk struct {
	ID      string               `json:"id"`              // ID is derived from the Responses response id.
	Object  string               `json:"object"`          // Object is always "chat.completion.chunk".
	Created int64                `json:"created"`         // Created is the Unix timestamp of the response.
	Model   string               `json:"model"`           // Model is the model identifier.
	Choices []chunkChoice        `json:"choices"`         // Choices holds the delta for this event.
	Usage   *chatCompletionUsage `json:"usage,omitempty"` // Usage is only set on the final usage-only chunk.
}

// chunkChoice is one choice slot in a streaming chunk (always index 0 here).
type chunkChoice struct {
	Index        int        `json:"index"`         // Index is always 0 for single-choice responses.
	Delta        chunkDelta `json:"delta"`         // Delta is the incremental content for this chunk.
	FinishReason *string    `json:"finish_reason"` // FinishReason is null for non-final chunks.
}

// chunkDelta is the incremental payload in a streaming chunk.
type chunkDelta struct {
	Content   *string              `json:"content,omitempty"`    // Content is the text delta; omitted when absent.
	ToolCalls []chunkToolCallDelta `json:"tool_calls,omitempty"` // ToolCalls is the function-call delta; omitted when absent.
}

// chunkToolCallDelta is a streamed function call slot in a chunk delta.
type chunkToolCallDelta struct {
	Index    int            `json:"index"`          // Index is the 0-based sequential position in the tool_calls array.
	ID       string         `json:"id,omitempty"`   // ID is set only on the introducing chunk for each call.
	Type     string         `json:"type,omitempty"` // Type is "function"; set only on the introducing chunk.
	Function chunkFuncDelta `json:"function"`       // Function holds the name and/or argument delta.
}

// chunkFuncDelta carries the function name and argument string delta in a streaming chunk.
type chunkFuncDelta struct {
	Name      string `json:"name,omitempty"` // Name is set only on the introducing chunk.
	Arguments string `json:"arguments"`      // Arguments is the delta fragment (empty string on intro chunk).
}

// streamState holds the mutable translation state for one streaming response.
type streamState struct {
	id             string         // id is the Responses response id extracted from response.created.
	model          string         // model is the model identifier extracted from response.created.
	created        int64          // created is the Unix timestamp used for all emitted chunks.
	hasToolCall    bool           // hasToolCall is true once any function-call item has been emitted.
	includeUsage   bool           // includeUsage controls emission of the final usage-only chunk.
	usage          responsesUsage // usage is populated from response.completed.
	toolCallCount  int            // toolCallCount is the number of function-call items introduced so far.
	toolCallIndex  map[int]int    // toolCallIndex maps Responses output_index to the 0-based sequential client index.
}

// toUsage converts the accumulated Responses API token counts to domain usage.
func (s *streamState) toUsage() usage.Usage {
	return usage.Usage{InputTokens: s.usage.InputTokens, OutputTokens: s.usage.OutputTokens}
}

// assignToolIndex registers a new function-call item identified by outputIndex and returns its
// 0-based sequential position in the tool_calls array, incrementing the counter for subsequent calls.
func (s *streamState) assignToolIndex(outputIndex int) int {
	if s.toolCallIndex == nil {
		s.toolCallIndex = make(map[int]int)
	}
	idx := s.toolCallCount
	s.toolCallCount++
	s.toolCallIndex[outputIndex] = idx
	return idx
}

// responsesStreamEvent is the minimal shape of a Responses API SSE event, covering all
// event types encountered during streaming translation.
type responsesStreamEvent struct {
	Type        string          `json:"type"`             // Type is the event type (e.g. "response.output_text.delta").
	OutputIndex int             `json:"output_index"`     // OutputIndex maps to the tool-call index.
	Delta       string          `json:"delta"`            // Delta is the text or argument fragment.
	Item        *streamAddedItem `json:"item,omitempty"`  // Item is present on response.output_item.added.
	Response    *streamRespMeta  `json:"response,omitempty"` // Response is present on response.created and response.completed.
}

// streamAddedItem is the item object inside a response.output_item.added event.
type streamAddedItem struct {
	Type   string `json:"type"`    // Type is "message", "function_call", or "reasoning".
	CallID string `json:"call_id"` // CallID is the function-call identifier for function_call items.
	Name   string `json:"name"`    // Name is the function identifier for function_call items.
}

// streamRespMeta is the minimal response object parsed from response.created and
// response.completed — enough to extract id, model, and usage without the full output array.
type streamRespMeta struct {
	ID    string          `json:"id"`            // ID is the Responses API response identifier.
	Model string          `json:"model"`         // Model is the model identifier.
	Usage *responsesUsage `json:"usage,omitempty"` // Usage is present on response.completed only.
}

// relayTranslatedStream reads the Responses API SSE stream from upstream, translates each
// relevant event to a chat.completion.chunk SSE line, and writes it to out. Events carrying
// Codex instructions (response.created, response.in_progress) and all reasoning events are
// silently dropped. The stream is terminated with "data: [DONE]". includeUsage controls
// whether a usage-only chunk is appended before [DONE]. The usage accumulated from
// response.completed is returned.
func relayTranslatedStream(upstream io.Reader, out domain.StreamSink, includeUsage bool) (usage.Usage, error) {
	reader := bufio.NewReader(upstream)
	state := &streamState{created: time.Now().Unix(), includeUsage: includeUsage}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if writeErr := dispatchStreamLine(line, out, state); writeErr != nil {
				return state.toUsage(), writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			out.Flush()
			return state.toUsage(), fmt.Errorf("read upstream SSE:\n%w", err)
		}
	}

	if err := emitDone(out); err != nil {
		return state.toUsage(), err
	}
	return state.toUsage(), nil
}

// dispatchStreamLine parses one SSE data line and routes its payload to the appropriate handler.
// Non-data lines (event:, blank lines, comments) and the terminal [DONE] are silently ignored.
func dispatchStreamLine(line []byte, out domain.StreamSink, state *streamState) error {
	data, ok := sseDataBytes(line)
	if !ok || string(data) == "[DONE]" {
		return nil
	}
	var event responsesStreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil // tolerate keep-alive comments and unexpected payloads
	}
	return handleStreamEvent(event, out, state)
}

// handleStreamEvent dispatches a parsed Responses SSE event to the correct emitter or drops it.
// response.created and response.in_progress are dropped (they carry Codex instructions).
// All reasoning events and item-done events are also dropped.
func handleStreamEvent(event responsesStreamEvent, out domain.StreamSink, state *streamState) error {
	switch event.Type {
	case "response.created":
		extractStreamMeta(event, state)
		return nil // drop; carries full Codex instructions
	case "response.in_progress":
		return nil // drop; also carries instructions
	case "response.output_text.delta":
		return emitContentDelta(event.Delta, out, state)
	case "response.output_item.added":
		if event.Item != nil && event.Item.Type == "function_call" {
			return emitFuncItemAdded(event, out, state)
		}
		return nil // drop reasoning and message item additions
	case "response.function_call_arguments.delta":
		return emitFuncArgsDelta(event, out, state)
	case "response.completed":
		return handleCompletedEvent(event, out, state)
	default:
		return nil // drop output_text.done, function_call_arguments.done, output_item.done, etc.
	}
}

// extractStreamMeta copies the response id and model from a response.created event into state
// so they can be echoed on every subsequent chunk.
func extractStreamMeta(event responsesStreamEvent, state *streamState) {
	if event.Response == nil {
		return
	}
	state.id = event.Response.ID
	state.model = event.Response.Model
}

// emitContentDelta emits a chat.completion.chunk carrying a text content delta.
func emitContentDelta(delta string, out domain.StreamSink, state *streamState) error {
	chunk := completionChunk{
		ID:      "chatcmpl-" + state.id,
		Object:  "chat.completion.chunk",
		Created: state.created,
		Model:   state.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Content: &delta}}},
	}
	return emitChunk(chunk, out)
}

// emitFuncItemAdded emits the introducing chunk for a function-call streaming item, carrying
// the call id, function name, and an empty arguments string. Sets state.hasToolCall.
func emitFuncItemAdded(event responsesStreamEvent, out domain.StreamSink, state *streamState) error {
	state.hasToolCall = true
	tc := chunkToolCallDelta{
		Index:    state.assignToolIndex(event.OutputIndex),
		ID:       event.Item.CallID,
		Type:     "function",
		Function: chunkFuncDelta{Name: event.Item.Name, Arguments: ""},
	}
	chunk := completionChunk{
		ID:      "chatcmpl-" + state.id,
		Object:  "chat.completion.chunk",
		Created: state.created,
		Model:   state.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{ToolCalls: []chunkToolCallDelta{tc}}}},
	}
	return emitChunk(chunk, out)
}

// emitFuncArgsDelta emits a chunk carrying a function-arguments delta fragment.
func emitFuncArgsDelta(event responsesStreamEvent, out domain.StreamSink, state *streamState) error {
	tc := chunkToolCallDelta{
		Index:    state.toolCallIndex[event.OutputIndex],
		Function: chunkFuncDelta{Arguments: event.Delta},
	}
	chunk := completionChunk{
		ID:      "chatcmpl-" + state.id,
		Object:  "chat.completion.chunk",
		Created: state.created,
		Model:   state.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{ToolCalls: []chunkToolCallDelta{tc}}}},
	}
	return emitChunk(chunk, out)
}

// handleCompletedEvent processes response.completed: updates state usage and id/model if not
// already set, then emits the finish chunk and optionally a usage-only chunk.
func handleCompletedEvent(event responsesStreamEvent, out domain.StreamSink, state *streamState) error {
	if event.Response != nil {
		if event.Response.Usage != nil {
			state.usage = *event.Response.Usage
		}
		if state.id == "" {
			state.id = event.Response.ID
		}
		if state.model == "" {
			state.model = event.Response.Model
		}
	}
	if err := emitFinishChunk(out, state); err != nil {
		return err
	}
	if state.includeUsage {
		return emitUsageChunk(out, state)
	}
	return nil
}

// emitFinishChunk emits the final chunk with finish_reason set to "tool_calls" when any
// function call was streamed, or "stop" otherwise.
func emitFinishChunk(out domain.StreamSink, state *streamState) error {
	reason := "stop"
	if state.hasToolCall {
		reason = "tool_calls"
	}
	chunk := completionChunk{
		ID:      "chatcmpl-" + state.id,
		Object:  "chat.completion.chunk",
		Created: state.created,
		Model:   state.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: &reason}},
	}
	return emitChunk(chunk, out)
}

// emitUsageChunk emits a usage-only chunk (choices: []) as specified by stream_options.include_usage.
func emitUsageChunk(out domain.StreamSink, state *streamState) error {
	u := chatCompletionUsage{
		PromptTokens:     state.usage.InputTokens,
		CompletionTokens: state.usage.OutputTokens,
		TotalTokens:      state.usage.InputTokens + state.usage.OutputTokens,
	}
	chunk := completionChunk{
		ID:      "chatcmpl-" + state.id,
		Object:  "chat.completion.chunk",
		Created: state.created,
		Model:   state.model,
		Choices: []chunkChoice{},
		Usage:   &u,
	}
	return emitChunk(chunk, out)
}

// emitChunk marshals chunk to JSON, writes it as an SSE data line, and flushes out to
// preserve streaming latency.
func emitChunk(chunk completionChunk, out domain.StreamSink) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("marshal chunk:\n%w", err)
	}
	if _, err := fmt.Fprintf(out, "data: %s\n\n", data); err != nil {
		return fmt.Errorf("write chunk:\n%w", err)
	}
	out.Flush()
	return nil
}

// emitDone writes the SSE stream terminator and flushes.
func emitDone(out domain.StreamSink) error {
	if _, err := fmt.Fprint(out, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("write DONE:\n%w", err)
	}
	out.Flush()
	return nil
}

// parseIncludeUsage extracts stream_options.include_usage from a raw Chat Completions request
// body, returning false when the field is absent or the body cannot be parsed.
func parseIncludeUsage(body []byte) bool {
	var opts struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &opts)
	return opts.StreamOptions.IncludeUsage
}
