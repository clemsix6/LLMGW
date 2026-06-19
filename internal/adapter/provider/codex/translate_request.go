package codex

import (
	"encoding/json"
	"fmt"
)

// validCodexModels is the set of model ids served by the Codex subscription backend,
// seeded in migration 0008. Unknown models are rejected at translation time.
var validCodexModels = map[string]bool{
	"gpt-5":       true,
	"gpt-5-codex": true,
	"gpt-5.5":     true,
}

// chatBody is the full Chat Completions request body parsed for translation.
type chatBody struct {
	Model      string    `json:"model"`       // Model is the requested model id.
	MaxTokens  int       `json:"max_tokens"`  // MaxTokens maps to max_output_tokens in the Responses body.
	Messages   []chatMsg `json:"messages"`    // Messages is the conversation history to translate into input items.
	Tools      []chatTool `json:"tools"`      // Tools is the set of callable functions.
	ToolChoice any       `json:"tool_choice"` // ToolChoice controls which function may be called.
}

// chatMsg is one Chat Completions message.
type chatMsg struct {
	Role       string          `json:"role"`         // Role is "system", "user", "assistant", or "tool".
	Content    json.RawMessage `json:"content"`      // Content is a JSON string or content-part array.
	ToolCalls  []chatToolCall  `json:"tool_calls"`   // ToolCalls lists function invocations from assistant messages.
	ToolCallID string          `json:"tool_call_id"` // ToolCallID links a tool-role message to its function_call.
}

// chatTool is a Chat Completions tool definition.
type chatTool struct {
	Type     string       `json:"type"`     // Type is always "function".
	Function chatFunction `json:"function"` // Function carries the function definition.
}

// chatFunction is the function definition nested inside a Chat Completions tool.
type chatFunction struct {
	Name        string          `json:"name"`        // Name is the function identifier.
	Description string          `json:"description"` // Description explains what the function does.
	Parameters  json.RawMessage `json:"parameters"`  // Parameters is the JSON Schema for the function arguments.
}

// chatToolCall is one function invocation inside an assistant message's tool_calls array.
type chatToolCall struct {
	ID       string       `json:"id"`       // ID is the call identifier used to pair with the tool-role result.
	Type     string       `json:"type"`     // Type is always "function".
	Function chatCallFunc `json:"function"` // Function carries the invocation name and arguments.
}

// chatCallFunc holds the name and JSON-encoded arguments of a tool call.
type chatCallFunc struct {
	Name      string `json:"name"`      // Name is the called function's identifier.
	Arguments string `json:"arguments"` // Arguments is the JSON-encoded argument object.
}

// translateRequest converts a Chat Completions body to a Responses body per spec §5.2.
// instructions is the Codex system prompt written into the Responses instructions field.
func translateRequest(body []byte, instructions string) ([]byte, error) {
	var cc chatBody
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, fmt.Errorf("parse chat completions body:\n%w", err)
	}
	model, err := validateModel(cc.Model)
	if err != nil {
		return nil, err
	}
	input, err := translateMessages(cc.Messages)
	if err != nil {
		return nil, err
	}
	req := responsesRequest{
		Model:           model,
		Instructions:    instructions,
		Input:           input,
		Store:           false,
		Stream:          true,
		MaxOutputTokens: cc.MaxTokens,
		Tools:           translateTools(cc.Tools),
		ToolChoice:      cc.ToolChoice,
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode responses body:\n%w", err)
	}
	return out, nil
}

// validateModel returns m unchanged when it is a known Codex-served model id, otherwise
// it returns an error naming the valid set.
func validateModel(m string) (string, error) {
	if validCodexModels[m] {
		return m, nil
	}
	return "", fmt.Errorf("unknown Codex model %q: must be one of gpt-5, gpt-5-codex, gpt-5.5", m)
}

// translateMessages maps every Chat Completions message to one or more Responses input items.
func translateMessages(messages []chatMsg) ([]responseItem, error) {
	items := make([]responseItem, 0, len(messages))
	for _, msg := range messages {
		translated, err := translateMessage(msg)
		if err != nil {
			return nil, err
		}
		items = append(items, translated...)
	}
	return items, nil
}

// translateMessage dispatches one Chat Completions message to the appropriate item builder.
func translateMessage(msg chatMsg) ([]responseItem, error) {
	switch msg.Role {
	case "system", "developer":
		return translateDeveloperMessage(msg)
	case "tool":
		return translateToolResult(msg), nil
	default:
		return translateChatMessage(msg)
	}
}

// translateDeveloperMessage maps a system or developer message to a developer input item.
// The client's own system message goes into input (not into the instructions field).
func translateDeveloperMessage(msg chatMsg) ([]responseItem, error) {
	content, err := parseTextContent(msg.Content, "input_text")
	if err != nil {
		return nil, fmt.Errorf("parse developer message content:\n%w", err)
	}
	return []responseItem{{Type: "message", Role: "developer", Content: content}}, nil
}

// translateChatMessage maps a user or assistant message. An assistant message with
// tool_calls yields additional function_call items appended after the text item (if any).
func translateChatMessage(msg chatMsg) ([]responseItem, error) {
	contentType := "input_text"
	if msg.Role == "assistant" {
		contentType = "output_text"
	}
	content, err := parseTextContent(msg.Content, contentType)
	if err != nil {
		return nil, fmt.Errorf("parse %s message content:\n%w", msg.Role, err)
	}
	var items []responseItem
	if len(content) > 0 {
		items = append(items, responseItem{Type: "message", Role: msg.Role, Content: content})
	}
	for _, tc := range msg.ToolCalls {
		items = append(items, translateToolCall(tc))
	}
	return items, nil
}

// translateToolCall maps one assistant tool_call to a function_call input item.
func translateToolCall(tc chatToolCall) responseItem {
	return responseItem{
		Type:      "function_call",
		CallID:    tc.ID,
		Name:      tc.Function.Name,
		Arguments: tc.Function.Arguments,
	}
}

// translateToolResult maps a tool-role message to a function_call_output input item.
func translateToolResult(msg chatMsg) []responseItem {
	output, _ := parseStringContent(msg.Content)
	return []responseItem{{
		Type:   "function_call_output",
		CallID: msg.ToolCallID,
		Output: output,
	}}
}

// translateTools maps Chat Completions tool definitions to Responses function tools.
// Returns nil when tools is empty so the field is omitted from the JSON output.
func translateTools(tools []chatTool) []responseTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responseTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, responseTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}

// parseTextContent parses a raw JSON content value (string or content-part array) into
// responseContent parts tagged with contentType ("input_text" or "output_text").
func parseTextContent(raw json.RawMessage, contentType string) ([]responseContent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if s, ok := parseStringContent(raw); ok {
		if s == "" {
			return nil, nil
		}
		return []responseContent{{Type: contentType, Text: s}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("parse content array:\n%w", err)
	}
	result := make([]responseContent, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			result = append(result, responseContent{Type: contentType, Text: p.Text})
		}
	}
	return result, nil
}

// parseStringContent attempts to unmarshal raw JSON as a Go string, returning the value
// and true on success.
func parseStringContent(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
