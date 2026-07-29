package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
)

type commandHandler func(context.Context, []string, Streams) error

var commandHandlers = map[string]commandHandler{
	"serve":  runServe,
	"auth":   runAuth,
	"key":    runKey,
	"budget": runBudget,
	"usage":  runUsage,
}

// Run parses global flags and dispatches one local command. With no command it serves.
func Run(ctx context.Context, args []string, streams Streams) error {
	streams = normalizedStreams(streams)
	root := flag.NewFlagSet("llmgw", flag.ContinueOnError)
	root.SetOutput(streams.Err)
	path := root.String("config", streams.ConfigPath, "shared YAML configuration path")
	help := root.Bool("help", false, "show help")
	root.Usage = func() { writeRootUsage(streams) }
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *help {
		writeRootUsage(streams)
		return nil
	}

	streams.ConfigPath = *path
	streams.ConfigPath = configPath(streams)
	command := "serve"
	leafArgs := root.Args()
	if len(leafArgs) > 0 {
		command, leafArgs = leafArgs[0], leafArgs[1:]
	}
	handler, ok := commandHandlers[command]
	if !ok {
		writeRootUsage(streams)
		return fmt.Errorf("unknown command %q", command)
	}
	return handler(ctx, leafArgs, streams)
}

func writeRootUsage(streams Streams) {
	fmt.Fprintln(streams.Out, `usage: llmgw [--config PATH] [serve]
       llmgw [--config PATH] auth ...
       llmgw [--config PATH] key ...
       llmgw [--config PATH] budget ...
       llmgw [--config PATH] usage ...

commands: serve, auth, key, budget, usage`)
}
