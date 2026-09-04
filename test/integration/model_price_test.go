package integration

import (
	"context"
	"testing"
)

// TestSeededHaikuPriceMatchesListPrice proves a database created from scratch
// prices claude-haiku-4-5 at Anthropic's published rates, on the undated
// pattern and on the dated sibling providers actually report. A wrong rate
// here never fails anything: attempts are costed, recorded and charged to a
// project at a price nobody publishes, and cost budgets act on the difference.
func TestSeededHaikuPriceMatchesListPrice(t *testing.T) {
	const query = `
SELECT input_per_million, output_per_million,
       cache_read_per_million, cache_creation_per_million
FROM model_price
WHERE model_pattern = $1`

	for _, pattern := range []string{"claude-haiku-4-5", "claude-haiku-4-5-*"} {
		var input, output, cacheRead, cacheCreation float64

		row := testHarness.db.QueryRow(context.Background(), query, pattern)
		if err := row.Scan(&input, &output, &cacheRead, &cacheCreation); err != nil {
			t.Fatalf("read seeded price of %q: %v", pattern, err)
		}

		if input != 1 || output != 5 || cacheRead != 0.1 || cacheCreation != 1.25 {
			t.Fatalf("seeded price of %q = %g/%g/%g/%g, want 1/5/0.1/1.25",
				pattern, input, output, cacheRead, cacheCreation)
		}
	}
}
