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

// TestCalculateRejectsUnknownAccounting verifies incomplete accounting is never priced.
func TestCalculateRejectsUnknownAccounting(t *testing.T) {
	completeRule := governance.PriceRule{
		Provider:                "provider",
		InputPerMillion:         floatPointer(1),
		OutputPerMillion:        floatPointer(1),
		CacheReadPerMillion:     floatPointer(1),
		CacheCreationPerMillion: floatPointer(1),
	}
	tests := []struct {
		name   string
		tokens governance.TokenBreakdown
		rule   governance.PriceRule
	}{
		{
			name:   "inconsistent total",
			tokens: governance.TokenBreakdown{Total: 3, UncachedInput: 1, Output: 1},
			rule:   completeRule,
		},
		{
			name:   "unclassified tokens",
			tokens: governance.TokenBreakdown{Total: 2, Unclassified: 2},
			rule:   completeRule,
		},
		{
			name:   "negative bucket",
			tokens: governance.TokenBreakdown{Total: -1, UncachedInput: -1},
			rule:   completeRule,
		},
		{
			name:   "missing used rate",
			tokens: governance.TokenBreakdown{Total: 1, Output: 1},
			rule: governance.PriceRule{
				Provider:        "provider",
				InputPerMillion: floatPointer(1),
			},
		},
		{
			name:   "no price rule",
			tokens: governance.TokenBreakdown{Total: 1, Output: 1},
			rule:   governance.PriceRule{},
		},
		{
			name:   "nan rate",
			tokens: governance.TokenBreakdown{Total: 1, Output: 1},
			rule: governance.PriceRule{
				Provider:         "provider",
				OutputPerMillion: floatPointer(math.NaN()),
			},
		},
		{
			name:   "infinite rate",
			tokens: governance.TokenBreakdown{Total: 1, Output: 1},
			rule: governance.PriceRule{
				Provider:         "provider",
				OutputPerMillion: floatPointer(math.Inf(1)),
			},
		},
		{
			name:   "infinite result",
			tokens: governance.TokenBreakdown{Total: math.MaxInt64, Output: math.MaxInt64},
			rule: governance.PriceRule{
				Provider:         "provider",
				OutputPerMillion: floatPointer(math.MaxFloat64),
			},
		},
		{
			name: "classified sum overflow",
			tokens: governance.TokenBreakdown{
				Total:         math.MaxInt64 - 2,
				UncachedInput: math.MaxInt64,
				CacheRead:     math.MaxInt64,
				CacheCreation: math.MaxInt64,
			},
			rule: completeRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, known := Calculate(test.tokens, test.rule); known || got != 0 {
				t.Fatalf("Calculate() = (%v, %t), want (0, false)", got, known)
			}
		})
	}
}

// floatPointer returns a stable pointer for a literal test price.
func floatPointer(value float64) *float64 {
	return &value
}
