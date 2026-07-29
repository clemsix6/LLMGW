package command

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
)

// TestBudgetCommandLifecycleRequiresExistingProjects catches a command mutation that creates a
// project through budget operations, loses a configured limit, or silently deletes no limit.
func TestBudgetCommandLifecycleRequiresExistingProjects(t *testing.T) {
	dsn := commandStore(t)
	streams := commandStreams(t, dsn)
	ctx := context.Background()
	if err := runBudget(ctx, []string{"set", "missing", "--dimension", "calls", "--window", "hour", "--max", "50", "--action", "block"}, streams); err == nil {
		t.Fatal("budget set created a missing project")
	}
	if err := runKey(ctx, []string{"create", "truewallet", "--name", "bootstrap"}, streams); err != nil {
		t.Fatalf("key create: %v", err)
	}
	streams.Out.(*bytes.Buffer).Reset()
	if err := runBudget(ctx, []string{"set", "truewallet", "--dimension", "calls", "--window", "hour", "--max", "50", "--action", "block"}, streams); err != nil {
		t.Fatalf("budget set: %v", err)
	}
	if !strings.Contains(streams.Out.(*bytes.Buffer).String(), "calls") {
		t.Fatal("set output omitted budget")
	}
	streams.Out.(*bytes.Buffer).Reset()
	if err := runBudget(ctx, []string{"list", "truewallet"}, streams); err != nil {
		t.Fatalf("budget list: %v", err)
	}
	listed := streams.Out.(*bytes.Buffer).String()
	if !strings.Contains(listed, "50") || !strings.Contains(listed, "block") {
		t.Fatalf("budget list = %q", listed)
	}
	match := regexp.MustCompile(`(?m)^id\s+([0-9]+)`).FindStringSubmatch(listed)
	if len(match) != 2 {
		t.Fatalf("budget list lacked id: %q", listed)
	}
	if err := runKey(ctx, []string{"create", "other-budget-project", "--name", "bootstrap"}, streams); err != nil {
		t.Fatalf("create second budget project: %v", err)
	}
	if err := runBudget(ctx, []string{"set", "other-budget-project", "--dimension", "cost", "--window", "day", "--max", "1.5", "--action", "warn"}, streams); err != nil {
		t.Fatalf("set second budget: %v", err)
	}
	streams.Out.(*bytes.Buffer).Reset()
	if err := runBudget(ctx, []string{"list"}, streams); err != nil {
		t.Fatalf("global budget list: %v", err)
	}
	global := streams.Out.(*bytes.Buffer).String()
	if len(regexp.MustCompile(`(?m)^id\t`).FindAllString(global, -1)) != 2 || !strings.Contains(global, "dimension\tcalls") || !strings.Contains(global, "dimension\tcost") || !strings.Contains(global, "max\t1.5") {
		t.Fatalf("global budget list = %q, want both exact limits", global)
	}
	if err := runBudget(ctx, []string{"delete", match[1]}, streams); err != nil {
		t.Fatalf("budget delete: %v", err)
	}
	store := openCommandStore(t, dsn)
	limits, err := store.ListBudgets(ctx, "truewallet")
	if err != nil || len(limits) != 0 {
		t.Fatalf("budgets after delete = (%#v, %v), want empty", limits, err)
	}
	other, err := store.ListBudgets(ctx, "other-budget-project")
	if err != nil || len(other) != 1 || other[0].Dimension != "cost" || other[0].MaxValue != 1.5 {
		t.Fatalf("unrelated budget after delete = (%#v, %v)", other, err)
	}
}
