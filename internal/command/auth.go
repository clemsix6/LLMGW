package command

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/config"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

var authLogin = cliproxy.Login

// runAuth executes a local provider-authentication leaf without starting the gateway.
func runAuth(ctx context.Context, args []string, streams Streams) error {
	streams = normalizedStreams(streams)
	if len(args) == 0 {
		return authUsage(streams, "missing auth command")
	}
	leaf, err := parseAuthLeaf(args, streams)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath(streams), streams.Getenv)
	if err != nil {
		return fmt.Errorf("load auth configuration:\n%w", err)
	}
	if err := cliproxy.PrepareAuthDir(cfg.Proxy.AuthDir); err != nil {
		return fmt.Errorf("prepare auth directory:\n%w", err)
	}
	return leaf(ctx, cfg, streams)
}

type authLeaf func(context.Context, config.Config, Streams) error

type authPriorityArguments struct {
	id       string
	priority int
}

type authLoginArguments struct {
	provider     string
	noBrowser    bool
	callbackPort int
	device       bool
}

// parseAuthLeaf validates every auth argument before configuration or filesystem access.
func parseAuthLeaf(args []string, streams Streams) (authLeaf, error) {
	switch args[0] {
	case "login":
		parsed, err := parseAuthLogin(args[1:], streams)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, cfg config.Config, streams Streams) error {
			return runAuthLogin(ctx, cfg, parsed, streams)
		}, nil
	case "list":
		if err := parseAuthNoArguments("auth list", args[1:], streams); err != nil {
			return nil, err
		}
		return runAuthList, nil
	case "import-legacy":
		if err := parseAuthNoArguments("auth import-legacy", args[1:], streams); err != nil {
			return nil, err
		}
		return runAuthImportLegacy, nil
	case "priority":
		parsed, err := parseAuthPriority(args[1:], streams)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, cfg config.Config, streams Streams) error {
			return runAuthPriority(ctx, cfg, parsed, streams)
		}, nil
	default:
		return nil, authUsage(streams, fmt.Sprintf("unknown auth command %q", args[0]))
	}
}

func parseAuthLogin(args []string, streams Streams) (authLoginArguments, error) {
	flags := flag.NewFlagSet("auth login", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	noBrowser := flags.Bool("no-browser", false, "do not open a browser")
	callbackPort := flags.Int("callback-port", 0, "OAuth callback port")
	device := flags.Bool("device", false, "use the Codex device flow")
	provider, err := parseRequiredTarget(flags, args)
	if err != nil {
		return authLoginArguments{}, err
	}
	if provider == "" || flags.NArg() != 0 || *callbackPort < 0 || !authProvider(provider) || (*device && provider != "codex") {
		return authLoginArguments{}, authUsage(streams, "auth login requires claude|codex|antigravity|kimi|xai; --device is Codex-only")
	}
	return authLoginArguments{
		provider: provider, noBrowser: *noBrowser, callbackPort: *callbackPort, device: *device,
	}, nil
}

// parseAuthPriority validates the credential ID and the integer priority.
// A negative priority is a last-resort tier and zero clears the priority.
func parseAuthPriority(args []string, streams Streams) (authPriorityArguments, error) {
	flags := flag.NewFlagSet("auth priority", flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	if err := flags.Parse(args); err != nil {
		return authPriorityArguments{}, err
	}
	if flags.NArg() != 2 {
		return authPriorityArguments{}, authUsage(streams, "auth priority requires ID and N")
	}
	priority, err := strconv.Atoi(flags.Arg(1))
	if err != nil {
		return authPriorityArguments{}, authUsage(streams, "auth priority N must be an integer")
	}
	return authPriorityArguments{id: flags.Arg(0), priority: priority}, nil
}

func parseAuthNoArguments(name string, args []string, streams Streams) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(streams.Err)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return authUsage(streams, name+" accepts no arguments")
	}
	return nil
}

func runAuthLogin(
	ctx context.Context,
	cfg config.Config,
	args authLoginArguments,
	streams Streams,
) error {
	// Resolved before the login runs, and in this leaf rather than in the auth
	// dispatch: a malformed webhook variable must leave auth list untouched.
	notifier, err := newOperatorNotifier(cfg, streams)
	if err != nil {
		return err
	}
	metadata := map[string]string(nil)
	if args.device {
		metadata = map[string]string{"codex_login_mode": "device"}
	}
	info, path, err := authLogin(ctx, cfg.Proxy, args.provider, &sdkauth.LoginOptions{
		NoBrowser: args.noBrowser, CallbackPort: args.callbackPort, Metadata: metadata, Prompt: authPrompt(streams),
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(streams.Out, "path\t%s\nprovider\t%s\nlabel\t%s\n", path, info.Provider, info.Label); err != nil {
		return fmt.Errorf("write auth login:\n%w", err)
	}
	notifier.emit(alert.KindCredentialAdded, credentialAddedFields(info)...)
	return nil
}

func runAuthList(ctx context.Context, cfg config.Config, streams Streams) error {
	auths, err := cliproxy.ListAuth(ctx, cfg.Proxy.AuthDir)
	if err != nil {
		return fmt.Errorf("list auth:\n%w", err)
	}
	for _, auth := range auths {
		if err := printAuthInfo(streams.Out, auth); err != nil {
			return fmt.Errorf("write auth list:\n%w", err)
		}
	}
	return nil
}

// runAuthPriority writes one credential's priority while holding the serve lock,
// so no running server can overwrite the file with its in-memory copy.
func runAuthPriority(
	ctx context.Context,
	cfg config.Config,
	args authPriorityArguments,
	streams Streams,
) error {
	dsn, err := cfg.DatabaseDSN(streams.Getenv)
	if err != nil {
		return fmt.Errorf("resolve auth database:\n%w", err)
	}
	store, err := postgres.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open auth store:\n%w", err)
	}
	defer store.Close()
	lock, err := store.AcquireServeLock(ctx)
	if errors.Is(err, postgres.ErrServeLockHeld) {
		return errors.New("auth priority refused: llmgw serve is running and would overwrite the change; stop the server, run the command, then start it again")
	}
	if err != nil {
		return fmt.Errorf("lock auth priority:\n%w", err)
	}
	defer lock.Release(context.WithoutCancel(ctx))
	info, err := cliproxy.SetAuthPriority(ctx, cfg.Proxy.AuthDir, args.id, args.priority)
	if err != nil {
		return err
	}
	return printAuthInfo(streams.Out, info)
}

// printAuthInfo writes one credential in the tab-separated form of auth list.
func printAuthInfo(out io.Writer, auth cliproxy.AuthInfo) error {
	_, err := fmt.Fprintf(
		out,
		"id\t%s\nprovider\t%s\nlabel\t%s\ndisabled\t%t\npriority\t%d\n",
		auth.ID, auth.Provider, auth.Label, auth.Disabled, auth.Priority,
	)
	if err != nil {
		return fmt.Errorf("write auth info:\n%w", err)
	}
	return nil
}

func runAuthImportLegacy(ctx context.Context, cfg config.Config, streams Streams) error {
	// Resolved before the import writes anything, and in this leaf rather than
	// in the auth dispatch: a malformed webhook variable must leave auth list
	// untouched.
	notifier, err := newOperatorNotifier(cfg, streams)
	if err != nil {
		return err
	}
	dsn, err := cfg.DatabaseDSN(streams.Getenv)
	if err != nil {
		return fmt.Errorf("resolve auth database:\n%w", err)
	}
	store, err := postgres.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open auth store:\n%w", err)
	}
	defer store.Close()
	credentials, err := store.LegacyCredentials(ctx)
	if err != nil {
		return fmt.Errorf("read legacy credentials:\n%w", err)
	}
	results, err := cliproxy.ImportLegacy(ctx, cfg.Proxy.AuthDir, credentials)
	if err != nil {
		return err
	}
	if err := printLegacyImports(streams.Out, results); err != nil {
		return err
	}
	notifier.emit(alert.KindCredentialsImported, importedCredentialsFields(results)...)
	return nil
}

// printLegacyImports writes the per-credential outcome of one legacy import.
func printLegacyImports(out io.Writer, results []cliproxy.LegacyImport) error {
	for _, result := range results {
		_, err := fmt.Fprintf(
			out,
			"provider\t%s\nlabel\t%s\nstatus\t%s\n",
			result.Provider, result.Label, result.Status,
		)
		if err != nil {
			return fmt.Errorf("write legacy import:\n%w", err)
		}
	}
	return nil
}

func authProvider(provider string) bool {
	switch provider {
	case "claude", "codex", "antigravity", "kimi", "xai":
		return true
	default:
		return false
	}
}

func authPrompt(streams Streams) func(string) (string, error) {
	reader := bufio.NewReader(streams.In)
	return func(prompt string) (string, error) {
		if _, err := io.WriteString(streams.Err, prompt); err != nil {
			return "", err
		}
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
}

func authUsage(streams Streams, message string) error {
	fmt.Fprintln(streams.Err, "usage: auth {login|list|import-legacy|priority ID N}")
	fmt.Fprintln(streams.Err, "  priority sets a credential's routing tier (0 clears, negative is last resort); it applies at the next serve start and is refused while a server runs")
	return fmt.Errorf("%s", message)
}
