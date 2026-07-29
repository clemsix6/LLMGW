package command

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/config"
)

const defaultConfigPath = "./config.yaml"

// Streams supplies controlled process I/O, environment access, and the root-parsed config path
// to local administrative leaf commands.
type Streams struct {
	In         io.Reader           // In is the command input stream.
	Out        io.Writer           // Out receives successful command output.
	Err        io.Writer           // Err receives flag and usage output.
	Getenv     func(string) string // Getenv resolves environment values.
	ConfigPath string              // ConfigPath is the root-parsed shared configuration path.
}

// parseOptionalTarget parses a leaf with zero or one positional target before or after flags.
func parseOptionalTarget(flags *flag.FlagSet, args []string) (string, error) {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if target == "" && flags.NArg() > 0 {
		target = flags.Arg(0)
		args = flags.Args()[1:]
		return target, flags.Parse(args)
	}
	return target, nil
}

// parseRequiredTarget parses a leaf with one positional target before or after flags.
func parseRequiredTarget(flags *flag.FlagSet, args []string) (string, error) {
	return parseOptionalTarget(flags, args)
}

// openStore loads the local configuration and opens its PostgreSQL state store.
func openStore(ctx context.Context, path string, streams Streams) (config.Config, *postgres.Store, error) {
	streams = normalizedStreams(streams)
	cfg, err := config.Load(path, streams.Getenv)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("load command configuration:\n%w", err)
	}
	dsn, err := cfg.DatabaseDSN(streams.Getenv)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("resolve command database:\n%w", err)
	}
	store, err := postgres.New(ctx, dsn)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("open command store:\n%w", err)
	}
	return cfg, store, nil
}

// configPath resolves the shared configuration path with root injection taking precedence.
func configPath(streams Streams) string {
	if streams.ConfigPath != "" {
		return streams.ConfigPath
	}
	streams = normalizedStreams(streams)
	if path := streams.Getenv("LLMGW_CONFIG"); path != "" {
		return path
	}
	return defaultConfigPath
}

// normalizedStreams fills optional streams with ordinary local process defaults.
func normalizedStreams(streams Streams) Streams {
	if streams.In == nil {
		streams.In = os.Stdin
	}
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	if streams.Getenv == nil {
		streams.Getenv = os.Getenv
	}
	return streams
}

// requireProject rejects an administrative target that has not been created by key create.
func requireProject(ctx context.Context, store *postgres.Store, name string) error {
	exists, err := store.ProjectExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check project %q:\n%w", name, err)
	}
	if !exists {
		return fmt.Errorf("project %q does not exist", name)
	}
	return nil
}
