package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain/effort"
	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// runProject executes one local project command.
func runProject(ctx context.Context, args []string, streams Streams) error {
	streams = normalizedStreams(streams)
	if len(args) == 0 {
		return projectUsage(streams, "missing project command")
	}
	switch args[0] {
	case "list":
		return runProjectList(ctx, args[1:], streams)
	case "tool-prefix":
		return runProjectToolPrefix(ctx, args[1:], streams)
	case "effort":
		return runProjectEffort(ctx, args[1:], streams)
	default:
		return projectUsage(streams, fmt.Sprintf("unknown project command %q", args[0]))
	}
}

// runProjectList emits every project's name, creation time, and tool-name-prefix state.
func runProjectList(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("project list", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return projectUsage(streams, "project list accepts no arguments")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	projects, err := store.Projects(ctx)
	if err != nil {
		return fmt.Errorf("list projects:\n%w", err)
	}
	for _, project := range projects {
		if err := printProject(streams.Out, project); err != nil {
			return fmt.Errorf("write project list:\n%w", err)
		}
	}
	return nil
}

// runProjectToolPrefix enables or disables outbound tool-name namespacing for one existing project.
// It fails with a clear message rather than creating the project: implicit
// project creation stays a property of key create alone.
func runProjectToolPrefix(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("project tool-prefix", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return projectUsage(streams, "project tool-prefix requires NAME and on|off")
	}
	name := flags.Arg(0)
	enabled, ok := parseToolPrefixState(flags.Arg(1))
	if !ok {
		return projectUsage(streams, "project tool-prefix state must be on or off")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetProjectToolPrefix(ctx, name, enabled); err != nil {
		if errors.Is(err, postgres.ErrProjectNotFound) {
			return fmt.Errorf("project %q does not exist", name)
		}
		return fmt.Errorf("set project tool prefix:\n%w", err)
	}
	if _, err := fmt.Fprintf(streams.Out, "project\t%s\nprefix_tool_names\t%t\n", name, enabled); err != nil {
		return fmt.Errorf("write project tool prefix:\n%w", err)
	}
	return nil
}

// runProjectEffort sets or clears one existing project's default Anthropic
// thinking effort. It fails with a clear message rather than creating the
// project: implicit project creation stays a property of key create alone.
func runProjectEffort(ctx context.Context, args []string, streams Streams) error {
	flags := flag.NewFlagSet("project effort", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return projectUsage(streams, "project effort requires NAME and low|medium|high|xhigh|max|none")
	}
	name := flags.Arg(0)
	level, ok := effort.ParseLevel(flags.Arg(1))
	if !ok {
		return projectUsage(streams, "project effort level must be low, medium, high, xhigh, max, or none")
	}
	_, store, err := openStore(ctx, configPath(streams), streams)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetProjectDefaultEffort(ctx, name, level); err != nil {
		if errors.Is(err, postgres.ErrProjectNotFound) {
			return fmt.Errorf("project %q does not exist", name)
		}
		return fmt.Errorf("set project default effort:\n%w", err)
	}
	if _, err := fmt.Fprintf(streams.Out, "project\t%s\ndefault_effort\t%s\n", name, printedEffort(level)); err != nil {
		return fmt.Errorf("write project default effort:\n%w", err)
	}
	return nil
}

// parseToolPrefixState parses the literal on|off argument into a boolean,
// reporting whether the argument was recognized at all.
func parseToolPrefixState(state string) (enabled bool, ok bool) {
	switch state {
	case "on":
		return true, true
	case "off":
		return false, true
	default:
		return false, false
	}
}

// printProject emits one project's non-secret operator-facing fields.
func printProject(output io.Writer, project governance.Project) error {
	_, err := fmt.Fprintf(
		output,
		"name\t%s\ncreated_at\t%s\nprefix_tool_names\t%t\ndefault_effort\t%s\n",
		project.Name, project.CreatedAt.UTC().Format(time.RFC3339), project.PrefixToolNames,
		printedEffort(project.DefaultEffort),
	)
	return err
}

// printedEffort renders a stored effort level for operator output, showing
// "none" for the empty string that means the project carries no default.
func printedEffort(level string) string {
	if level == "" {
		return "none"
	}
	return level
}

// projectUsage writes a short usage line for a leaf-parser error.
func projectUsage(streams Streams, message string) error {
	fmt.Fprintln(streams.Err, "usage: project {list|tool-prefix|effort}")
	return fmt.Errorf("%s", message)
}
