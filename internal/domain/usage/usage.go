package usage

// Usage holds the token counts attributed to a single LLM request.
type Usage struct {
	InputTokens int // InputTokens is the number of prompt tokens consumed.

	OutputTokens int // OutputTokens is the number of generated tokens.
}
