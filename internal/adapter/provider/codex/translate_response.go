package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// completedEventEnvelope is the minimal shape of a response.completed SSE data payload,
// used to identify the event type and extract the raw response object JSON.
type completedEventEnvelope struct {
	Type     string          `json:"type"`     // Type is the SSE event type (e.g. "response.completed").
	Response json.RawMessage `json:"response"` // Response is the full response object JSON.
}

// responsesCompleted is the shape of the response object inside a response.completed event.
type responsesCompleted struct {
	ID        string            `json:"id"`         // ID is the Responses API response identifier.
	Model     string            `json:"model"`      // Model is the model identifier from the response.
	CreatedAt int64             `json:"created_at"` // CreatedAt is the Unix timestamp of response creation; 0 if absent.
	Output    []responsesOutput `json:"output"`     // Output is the ordered list of output items.
	Usage     responsesUsage    `json:"usage"`      // Usage holds the token counts for the call.
}

// responsesOutput is one item in the Responses API output array. The Type field determines
// which other fields are populated: "message" has Role+Content; "function_call" has
// CallID+Name+Arguments; "reasoning" items are always dropped.
type responsesOutput struct {
	Type      string                   `json:"type"`      // Type is "message", "function_call", or "reasoning".
	Role      string                   `json:"role"`      // Role is the speaker; set on "message" items.
	Content   []responsesOutputContent `json:"content"`   // Content is the parts list for "message" items.
	CallID    string                   `json:"call_id"`   // CallID links a "function_call" to its result.
	Name      string                   `json:"name"`      // Name is the function identifier for "function_call" items.
	Arguments string                   `json:"arguments"` // Arguments is the JSON-encoded args for "function_call" items.
}

// responsesOutputContent is one content part inside a Responses output message.
type responsesOutputContent struct {
	Type string `json:"type"` // Type is typically "output_text".
	Text string `json:"text"` // Text is the message text.
}

// responsesUsage holds the token counts from the Responses API, using its field names.
type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`  // InputTokens is the number of prompt tokens consumed.
	OutputTokens int `json:"output_tokens"` // OutputTokens is the number of generated tokens.
}

// chatCompletion is the Chat Completions response object the provider emits for non-streaming
// Codex calls, shaped to be indistinguishable from a real OpenAI response.
type chatCompletion struct {
	ID      string              `json:"id"`      // ID is derived from the Responses response id.
	Object  string              `json:"object"`  // Object is always "chat.completion".
	Model   string              `json:"model"`   // Model is the model identifier echoed from the Responses response.
	Created int64               `json:"created"` // Created is the Unix timestamp of the response.
	Choices []chatChoice        `json:"choices"` // Choices holds the single translated choice.
	Usage   chatCompletionUsage `json:"usage"`   // Usage maps Responses token counts to Chat Completions names.
}

// chatChoice is one choice in the Chat Completions response (always index 0 here).
type chatChoice struct {
	Index        int                   `json:"index"`         // Index is always 0 for single-choice responses.
	Message      chatCompletionMessage `json:"message"`       // Message is the translated assistant turn.
	FinishReason string                `json:"finish_reason"` // FinishReason is "stop" or "tool_calls".
}

// chatCompletionMessage is the assistant message in a Chat Completions response.
type chatCompletionMessage struct {
	Role      string         `json:"role"`                 // Role is always "assistant".
	Content   *string        `json:"content"`              // Content is the folded text; null when no text output.
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"` // ToolCalls lists function calls; omitted when none.
}

// chatCompletionUsage maps Responses API token counts to the Chat Completions field names.
type chatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`     // PromptTokens is the input token count.
	CompletionTokens int `json:"completion_tokens"` // CompletionTokens is the output token count.
	TotalTokens      int `json:"total_tokens"`      // TotalTokens is the sum of input and output tokens.
}

// aggregateCompleted reads the Responses SSE stream using an unbounded bufio.Reader (no 64 KB
// line limit) and returns the raw JSON of the response object from the response.completed event.
// All preceding events are discarded; the function errors if the event is not found before EOF.
func aggregateCompleted(upstream io.Reader) ([]byte, error) {
	reader := bufio.NewReader(upstream)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			result, scanErr := scanCompletedLine(line)
			if scanErr != nil {
				return nil, scanErr
			}
			if result != nil {
				return result, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read SSE stream:\n%w", err)
		}
	}
	return nil, fmt.Errorf("response.completed event not found in upstream SSE")
}

// scanCompletedLine checks whether line is a response.completed SSE data line and returns
// its embedded response JSON. Returns (nil, nil) for non-matching lines.
func scanCompletedLine(line []byte) ([]byte, error) {
	data, ok := sseDataBytes(line)
	if !ok {
		return nil, nil
	}
	var env completedEventEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.Type != "response.completed" {
		return nil, nil
	}
	if len(env.Response) == 0 {
		return nil, fmt.Errorf("response.completed event carries no response object")
	}
	return env.Response, nil
}

// sseDataBytes extracts the JSON payload from an SSE "data:" line, reporting false for
// event/blank/comment lines. It mirrors the claudemax sseData helper for the Responses format.
func sseDataBytes(line []byte) ([]byte, bool) {
	const prefix = "data:"
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte(prefix)) {
		return nil, false
	}
	return bytes.TrimSpace(trimmed[len(prefix):]), true
}

// translateResponse converts a Responses API response object to a Chat Completions JSON object
// per spec §5.3: folds output[] into one choice, drops reasoning items, maps finish_reason,
// and translates input_tokens/output_tokens to prompt_tokens/completion_tokens.
func translateResponse(completed []byte) ([]byte, usage.Usage, error) {
	var resp responsesCompleted
	if err := json.Unmarshal(completed, &resp); err != nil {
		return nil, usage.Usage{}, fmt.Errorf("parse completed response:\n%w", err)
	}
	content, toolCalls := foldOutput(resp.Output)
	u := usage.Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	created := resp.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	cc := buildChatCompletion(resp.ID, resp.Model, created, content, toolCalls, u)
	out, err := json.Marshal(cc)
	if err != nil {
		return nil, usage.Usage{}, fmt.Errorf("encode chat completion:\n%w", err)
	}
	return out, u, nil
}

// foldOutput iterates output items: text from "message" items is joined into a single string,
// "function_call" items become chatToolCalls, and "reasoning" items are silently dropped
// per the clean-output requirement (spec §5.3).
func foldOutput(output []responsesOutput) (*string, []chatToolCall) {
	var parts []string
	var calls []chatToolCall
	for _, item := range output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
		case "function_call":
			calls = append(calls, chatToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: chatCallFunc{Name: item.Name, Arguments: item.Arguments},
			})
			// "reasoning" items are intentionally absent from this switch (spec §5.3 DROP).
		}
	}
	if len(parts) == 0 {
		return nil, calls // null content when response has only tool_calls or reasoning
	}
	s := strings.Join(parts, "")
	return &s, calls
}

// buildChatCompletion assembles the Chat Completions response object from its translated parts.
func buildChatCompletion(respID, model string, created int64, content *string, toolCalls []chatToolCall, u usage.Usage) chatCompletion {
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	return chatCompletion{
		ID:      "chatcmpl-" + respID,
		Object:  "chat.completion",
		Model:   model,
		Created: created,
		Choices: []chatChoice{{
			Index: 0,
			Message: chatCompletionMessage{
				Role:      "assistant",
				Content:   content,
				ToolCalls: toolCalls,
			},
			FinishReason: finishReason,
		}},
		Usage: chatCompletionUsage{
			PromptTokens:     u.InputTokens,
			CompletionTokens: u.OutputTokens,
			TotalTokens:      u.InputTokens + u.OutputTokens,
		},
	}
}
