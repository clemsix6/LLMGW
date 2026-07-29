package command

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// runBudget executes one local project budget command.
func runBudget(ctx context.Context, args []string, streams Streams) error {
	streams = normalizedStreams(streams)
	if len(args) == 0 {
		return budgetUsage(streams, "missing budget command")
	}
	switch args[0] {
	case "set":
		return runBudgetSet(ctx, args[1:], streams)
	case "list":
		return runBudgetList(ctx, args[1:], streams)
	case "delete":
		return runBudgetDelete(ctx, args[1:], streams)
	default:
		return budgetUsage(streams, fmt.Sprintf("unknown budget command %q", args[0]))
	}
}

// runBudgetSet creates or replaces one limit on an existing project.
func runBudgetSet(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("budget set", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	dimension := flags.String("dimension", "", "calls, tokens, or cost")
	window := flags.String("window", "", "hour or day")
	maximum := flags.Float64("max", -1, "maximum usage")
	action := flags.String("action", "", "block or warn")
	project, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if project == "" || flags.NArg() != 0 || !validBudgetFlags(*dimension, *window, *maximum, *action) {
		return budgetUsage(streams, "budget set requires valid --dimension, --window, --max, and --action")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := requireProject(ctx, store, project); err != nil {
		return err
	}
	limit, err := store.SetBudget(ctx, project, governance.Dimension(*dimension), governance.Window(*window), *maximum, governance.Action(*action))
	if err != nil {
		return fmt.Errorf("set budget:\n%w", err)
	}
	if err := printBudget(streams.Out, limit); err != nil {
		return fmt.Errorf("write budget:\n%w", err)
	}
	return nil
}

// runBudgetList emits all limits for one existing project or every project.
func runBudgetList(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("budget list", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	project, err := parseOptionalTarget(flags, args)
	if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return budgetUsage(streams, "budget list accepts at most PROJECT")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if project != "" {
		if err := requireProject(ctx, store, project); err != nil {
			return err
		}
	}
	limits, err := store.ListBudgets(ctx, project)
	if err != nil {
		return fmt.Errorf("list budgets:\n%w", err)
	}
	for _, limit := range limits {
		if err := printBudget(streams.Out, limit); err != nil {
			return fmt.Errorf("write budget list:\n%w", err)
		}
	}
	return nil
}

// runBudgetDelete removes one existing budget by identifier.
func runBudgetDelete(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("budget delete", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	limitText, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if limitText == "" || flags.NArg() != 0 {
		return budgetUsage(streams, "budget delete requires LIMIT_ID")
	}
	limitID, err := strconv.ParseInt(limitText, 10, 64)
	if err != nil || limitID < 1 {
		return budgetUsage(streams, "budget delete requires a positive LIMIT_ID")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.DeleteBudget(ctx, limitID); err != nil {
		return fmt.Errorf("delete budget:\n%w", err)
	}
	if _, err := fmt.Fprintf(streams.Out, "id\t%d\ndeleted\ttrue\n", limitID); err != nil {
		return fmt.Errorf("write deleted budget:\n%w", err)
	}
	return nil
}

// validBudgetFlags selects only the persisted enumeration values and non-negative limits.
func validBudgetFlags(dimension, window string, maximum float64, action string) bool {
	return (dimension == "calls" || dimension == "tokens" || dimension == "cost") &&
		(window == "hour" || window == "day") && maximum >= 0 &&
		(action == "block" || action == "warn")
}

// printBudget emits one operator-facing non-secret budget limit.
func printBudget(output io.Writer, limit governance.BudgetLimit) error {
	_, err := fmt.Fprintf(output, "id\t%d\nproject_id\t%d\ndimension\t%s\nwindow\t%s\nmax\t%g\naction\t%s\n", limit.ID, limit.ProjectID, limit.Dimension, limit.Window, limit.MaxValue, limit.Action)
	return err
}

// budgetUsage writes a short usage line for a leaf-parser error.
func budgetUsage(streams Streams, message string) error {
	fmt.Fprintln(streams.Err, "usage: budget {set|list|delete}")
	return fmt.Errorf("%s", message)
}
