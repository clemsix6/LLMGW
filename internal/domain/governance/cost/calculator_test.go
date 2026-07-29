package cost

import (
	"math"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestCalculateCanonicalBuckets verifies reasoning remains an output breakdown.
func TestCalculateCanonicalBuckets(t *testing.T) {
	tokens := governance.TokenBreakdown{
		Total:         190,
		UncachedInput: 100,
		CacheRead:     20,
		CacheCreation: 10,
		Output:        60,
		Reasoning:     15,
	}
	rule := governance.PriceRule{
		Provider:                "provider",
		InputPerMillion:         floatPointer(3),
		OutputPerMillion:        floatPointer(15),
		CacheReadPerMillion:     floatPointer(0.3),
		CacheCreationPerMillion: floatPointer(3.75),
	}

	got, known := Calculate(tokens, rule)
	want := (100*3.0 + 20*0.3 + 10*3.75 + 60*15.0) / 1_000_000
	if !known {
		t.Fatal("canonical token cost is unknown")
	}
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

// floatPointer returns a stable pointer for a literal test price.
func floatPointer(value float64) *float64 {
	return &value
}
