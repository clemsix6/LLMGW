package httpserver

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// TestWindowDuration proves windowDuration maps "day" to 24h and every other window value
// (including the hourly case and any unexpected string) to 1h.
func TestWindowDuration(t *testing.T) {
	cases := []struct {
		window string
		want   time.Duration
	}{
		{"day", 24 * time.Hour},
		{"hour", time.Hour},
		{"", time.Hour},
		{"week", time.Hour},
	}

	for _, c := range cases {
		if got := windowDuration(c.window); got != c.want {
			t.Errorf("windowDuration(%q) = %v, want %v", c.window, got, c.want)
		}
	}
}

// TestCurrentForDimension proves currentForDimension sums recorded plus in-flight usage for each
// dimension (tokens combines input + output), and reports 0 for an unknown dimension.
func TestCurrentForDimension(t *testing.T) {
	totals := domain.WindowTotals{
		Current:  domain.Totals{Calls: 3, InputTokens: 100, OutputTokens: 40, CostUSD: 0.50},
		Inflight: domain.Totals{Calls: 2, InputTokens: 7, OutputTokens: 3, CostUSD: 0.25},
	}

	cases := []struct {
		dimension string
		want      float64
	}{
		{dimensionCalls, 5},      // 3 recorded + 2 in-flight
		{dimensionTokens, 150},   // (100+40) recorded + (7+3) in-flight
		{dimensionCostUSD, 0.75}, // 0.50 recorded + 0.25 in-flight
		{"unknown-dimension", 0}, // unmapped dimension reports nothing
	}

	for _, c := range cases {
		if got := currentForDimension(c.dimension, totals); got != c.want {
			t.Errorf("currentForDimension(%q) = %v, want %v", c.dimension, got, c.want)
		}
	}
}

// TestGroupLimits proves groupLimits buckets limits by (tag scope, window): limits sharing a
// scope and window land in one group, whole-project (nil-tag) groups read the project-wide
// aggregate while concrete-tag groups read the request tag, and each group's Since is now minus
// the window length. First-seen order is preserved.
func TestGroupLimits(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	news := "news"
	feed := "feed"

	// Ordered as LimitsFor returns them (by id). #1 and #3 share the (whole-project, hour) bucket.
	limits := []domain.BudgetLimit{
		{Tag: nil, Dimension: "calls", Window: "hour", MaxValue: 10, Action: "block"},
		{Tag: &feed, Dimension: "calls", Window: "hour", MaxValue: 5, Action: "block"},
		{Tag: nil, Dimension: "tokens", Window: "hour", MaxValue: 100, Action: "warn"},
		{Tag: nil, Dimension: "cost_usd", Window: "day", MaxValue: 5, Action: "block"},
	}

	groups := groupLimits(limits, news, now)

	if len(groups) != 3 {
		t.Fatalf("groupLimits returned %d groups, want 3 (whole-project/hour, feed/hour, whole-project/day)", len(groups))
	}

	// Group 0: whole-project + hour, holding both nil-tag hour limits, aggregating the project.
	assertGroup(t, groups[0], 2, domain.WholeProjectTag, now.Add(-time.Hour))

	// Group 1: concrete tag + hour, reading the request tag ("news"), not the limit's own tag.
	assertGroup(t, groups[1], 1, news, now.Add(-time.Hour))

	// Group 2: whole-project + day, with a 24h window start.
	assertGroup(t, groups[2], 1, domain.WholeProjectTag, now.Add(-24*time.Hour))
}

// assertGroup fails unless the group holds wantLen limits and its read targets wantTag since wantSince.
func assertGroup(t *testing.T, group limitGroup, wantLen int, wantTag string, wantSince time.Time) {
	t.Helper()

	if len(group.limits) != wantLen {
		t.Errorf("group has %d limits, want %d", len(group.limits), wantLen)
	}
	if group.read.Tag != wantTag {
		t.Errorf("group read tag = %q, want %q", group.read.Tag, wantTag)
	}
	if !group.read.Since.Equal(wantSince) {
		t.Errorf("group read since = %v, want %v", group.read.Since, wantSince)
	}
}
