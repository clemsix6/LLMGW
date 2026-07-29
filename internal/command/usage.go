package command

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// runUsage executes one local usage reporting or explicit accounting-resolution command.
func runUsage(ctx context.Context, args []string, streams Streams) error {
	streams = normalizedStreams(streams)
	if len(args) == 0 {
		return usageUsage(streams, "missing usage command")
	}
	switch args[0] {
	case "show":
		return runUsageShow(ctx, args[1:], streams)
	case "resolve":
		return runUsageResolve(ctx, args[1:], streams)
	default:
		return usageUsage(streams, fmt.Sprintf("unknown usage command %q", args[0]))
	}
}

// runUsageShow prints usage grouped through the repository's fixed SQL whitelist.
func runUsageShow(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("usage show", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	since := flags.Duration("since", 0, "lookback duration")
	groupBy := flags.String("by", "", "key, model, or provider")
	project, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if project == "" || flags.NArg() != 0 || *since <= 0 || !validUsageGroup(*groupBy) {
		return usageUsage(streams, "usage show requires positive --since and --by key|model|provider")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := requireProject(ctx, store, project); err != nil {
		return err
	}
	summaries, err := store.QueryUsage(ctx, governance.UsageQuery{Project: project, Since: time.Now().UTC().Add(-*since), GroupBy: *groupBy})
	if err != nil {
		return fmt.Errorf("show usage:\n%w", err)
	}
	for _, summary := range summaries {
		if err := printUsageSummary(streams.Out, summary); err != nil {
			return fmt.Errorf("write usage report:\n%w", err)
		}
	}
	return nil
}

// runUsageResolve requires the literal consent flag before the guarded zero-accounting transition.
func runUsageResolve(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("usage resolve", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	assumeZero := flags.Bool("assume-zero", false, "confirm zero usage resolution")
	requestID, err := parseRequiredTarget(flags, args)
	if err != nil {
		return err
	}
	if requestID == "" || flags.NArg() != 0 || !*assumeZero || !literalAssumeZero(args) {
		return usageUsage(streams, "usage resolve requires literal --assume-zero")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	resolvedAt := time.Now().UTC()
	if err := store.ResolveUnknownAsZero(ctx, requestID, resolvedAt); err != nil {
		return fmt.Errorf("resolve usage:\n%w", err)
	}
	if _, err := fmt.Fprintf(streams.Out, "request_id\t%s\nold_state\taccounting_unknown\nnew_state\tresolved_zero\nresolved_at\t%s\n", requestID, resolvedAt.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write usage resolution:\n%w", err)
	}
	return nil
}

// literalAssumeZero rejects value forms so the destructive accounting decision is unmistakable.
func literalAssumeZero(args []string) bool {
	for _, argument := range args {
		if argument == "--assume-zero" {
			return true
		}
	}
	return false
}

// validUsageGroup mirrors the SQL reporting whitelist before database work begins.
func validUsageGroup(group string) bool {
	return group == "key" || group == "model" || group == "provider"
}

// printUsageSummary emits aggregate values only, never provider credential identifiers.
func printUsageSummary(output io.Writer, summary governance.UsageSummary) error {
	_, err := fmt.Fprintf(output, "group\t%s\ncalls\t%d\ntokens\t%d\ncost_usd\t%g\nfailed_attempts\t%d\nunknown_pricing\t%d\nunknown_accounting\t%d\n", summary.Group, summary.Calls, summary.Tokens, summary.CostUSD, summary.FailedAttempts, summary.UnknownPricing, summary.UnknownAccounting)
	return err
}

// usageUsage writes a short usage line for a leaf-parser error.
func usageUsage(streams Streams, message string) error {
	fmt.Fprintln(streams.Err, "usage: usage {show|resolve}")
	return fmt.Errorf("%s", message)
}
