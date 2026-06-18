package usage

import "testing"

// TestCost checks the notional cost arithmetic across the per-million-token tiers, including
// zero usage, single-sided usage, and sub-million counts that must scale linearly.
func TestCost(t *testing.T) {
	tests := []struct {
		name string  // name describes the scenario.
		u    Usage   // u is the metered token counts.
		in   float64 // in is the input price per million tokens.
		out  float64 // out is the output price per million tokens.
		want float64 // want is the expected USD cost.
	}{
		{"zero usage", Usage{}, 15, 75, 0},
		{"input only, 1M @ 3", Usage{InputTokens: 1_000_000}, 3, 15, 3},
		{"output only, 1M @ 15", Usage{OutputTokens: 1_000_000}, 3, 15, 15},
		{"sonnet 1M in + 1M out", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, 3, 15, 18},
		{"opus partial", Usage{InputTokens: 500_000, OutputTokens: 200_000}, 15, 75, 7.5 + 15},
		{"haiku sub-million", Usage{InputTokens: 1_500, OutputTokens: 800}, 0.80, 4, 1500.0/1e6*0.80 + 800.0/1e6*4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cost(tt.u, tt.in, tt.out)
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Cost(%+v, %v, %v) = %v, want %v", tt.u, tt.in, tt.out, got, tt.want)
			}
		})
	}
}
