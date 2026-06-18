package llm

// ChatRequest is a single Anthropic Messages request flowing through the gateway.
// V1 keeps the body unchanged except for a prepended Claude Code system block, so the
// request stays compatible with any Anthropic SDK.
//
// TODO (Batch 2): add parsing and typed accessors (ParseRequest, Model, Stream,
// FirstUserText, WithClaudeCodeSystem, Bytes). For now it is a stable forward
// declaration so the Provider port can be defined.
type ChatRequest struct {
	raw []byte // raw is the original Anthropic request body, forwarded unchanged.
}
