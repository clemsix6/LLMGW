package integration

import (
	"context"
	"testing"
)

// seededListPrice is the price a database created from scratch must carry for
// one model pattern, on the undated pattern and on the dated sibling providers
// actually report.
type seededListPrice struct {
	pattern       string  // pattern is the model_price row's model_pattern.
	input         float64 // input is the published input rate per million tokens.
	output        float64 // output is the published output rate per million tokens.
	cacheRead     float64 // cacheRead is the published cache-read rate per million tokens.
	cacheCreation float64 // cacheCreation is the published cache-creation rate per million tokens.
}

// TestSeededPricesMatchListPrice proves a database created from scratch prices
// the corrected models at Anthropic's published rates. A wrong rate here never
// fails anything: attempts are costed, recorded and charged to a project at a
// price nobody publishes, and cost budgets act on the difference.
func TestSeededPricesMatchListPrice(t *testing.T) {
	const query = `
SELECT input_per_million, output_per_million,
       cache_read_per_million, cache_creation_per_million
FROM model_price
WHERE model_pattern = $1 AND provider = '*'`

	expected := []seededListPrice{
		{pattern: "claude-haiku-4-5", input: 1, output: 5, cacheRead: 0.1, cacheCreation: 1.25},
		{pattern: "claude-haiku-4-5-*", input: 1, output: 5, cacheRead: 0.1, cacheCreation: 1.25},
		{pattern: "claude-opus-4-8", input: 5, output: 25, cacheRead: 0.5, cacheCreation: 6.25},
		{pattern: "claude-opus-4-8-*", input: 5, output: 25, cacheRead: 0.5, cacheCreation: 6.25},
	}

	for _, want := range expected {
		var input, output, cacheRead, cacheCreation float64

		row := testHarness.db.QueryRow(context.Background(), query, want.pattern)
		if err := row.Scan(&input, &output, &cacheRead, &cacheCreation); err != nil {
			t.Fatalf("read seeded price of %q: %v", want.pattern, err)
		}

		if input != want.input || output != want.output ||
			cacheRead != want.cacheRead || cacheCreation != want.cacheCreation {
			t.Fatalf("seeded price of %q = %g/%g/%g/%g, want %g/%g/%g/%g",
				want.pattern, input, output, cacheRead, cacheCreation,
				want.input, want.output, want.cacheRead, want.cacheCreation)
		}
	}
}
