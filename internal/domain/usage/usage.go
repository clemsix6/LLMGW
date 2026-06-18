package usage

// Usage holds the token counts attributed to a single LLM request.
type Usage struct {
	InputTokens int // InputTokens is the number of prompt tokens consumed.

	OutputTokens int // OutputTokens is the number of generated tokens.
}

// Cost returns the notional USD cost of a call given the per-million-token input and output
// prices: inputTokens/1e6*in + outputTokens/1e6*out.
func Cost(u Usage, inUSDPerMTok, outUSDPerMTok float64) float64 {
	return float64(u.InputTokens)/1e6*inUSDPerMTok + float64(u.OutputTokens)/1e6*outUSDPerMTok
}
