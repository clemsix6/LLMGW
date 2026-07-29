package command

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestCommandConfigPathPrecedence catches a configuration resolver mutation that ignores the
// injected leaf path or environment fallback and opens a different local configuration.
func TestCommandConfigPathPrecedence(t *testing.T) {
	defaultPath := filepath.Join(t.TempDir(), "config.yaml")
	envPath := filepath.Join(t.TempDir(), "env.yaml")
	explicitPath := filepath.Join(t.TempDir(), "explicit.yaml")
	for _, path := range []string{defaultPath, envPath, explicitPath} {
		if err := os.WriteFile(path, []byte(commandConfig), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	tests := []struct {
		name    string
		streams Streams
		want    string
	}{
		{name: "default", streams: testStreams(nil), want: "./config.yaml"},
		{name: "environment", streams: testStreams(map[string]string{"LLMGW_CONFIG": envPath}), want: envPath},
		{name: "injected", streams: Streams{Getenv: func(string) string { return envPath }, ConfigPath: explicitPath}, want: explicitPath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "default" {
				old, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chdir(filepath.Dir(defaultPath)); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chdir(old) })
			}
			path := configPath(test.streams)
			if path != test.want {
				t.Fatalf("config path = %q, want %q", path, test.want)
			}
		})
	}
}

// TestCommandConfigPrecedencePerCommand catches a mutation that makes any command family ignore
// Streams.ConfigPath, LLMGW_CONFIG, or the ./config.yaml fallback while opening real PostgreSQL.
func TestCommandConfigPrecedencePerCommand(t *testing.T) {
	dsn := commandStore(t)
	store := openCommandStore(t, dsn)
	ctx := context.Background()
	key, err := store.CreateKey(ctx, "precedence", "seed", "precedence-public", make([]byte, 32), nil)
	if err != nil {
		t.Fatalf("seed precedence key: %v", err)
	}
	if _, err := store.SetBudget(ctx, "precedence", governance.DimensionCalls, governance.WindowHour, 7, governance.ActionBlock); err != nil {
		t.Fatalf("seed precedence budget: %v", err)
	}
	if key.ProjectID == 0 {
		t.Fatal("seed project was not persisted")
	}

	validDirectory := t.TempDir()
	validPath := filepath.Join(validDirectory, "config.yaml")
	writeCommandConfig(t, validPath)
	missingPath := filepath.Join(t.TempDir(), "lower-priority-must-not-open.yaml")
	commands := []struct {
		name string
		run  func(context.Context, []string, Streams) error
		args []string
	}{
		{name: "key", run: runKey, args: []string{"list", "precedence"}},
		{name: "budget", run: runBudget, args: []string{"list", "precedence"}},
		{name: "usage", run: runUsage, args: []string{"show", "precedence", "--since", "24h", "--by", "key"}},
	}
	for _, command := range commands {
		for _, mode := range []string{"explicit", "environment", "default"} {
			t.Run(command.name+"/"+mode, func(t *testing.T) {
				streams := Streams{Out: new(bytes.Buffer), Err: new(bytes.Buffer)}
				streams.Getenv = func(name string) string {
					switch name {
					case "TEST_DSN":
						return dsn
					case "TEST_PEPPER":
						panic("non-creation command loaded key pepper")
					case "LLMGW_CONFIG":
						if mode == "explicit" {
							return missingPath
						}
						if mode == "environment" {
							return validPath
						}
					}
					return ""
				}
				workingDirectory := t.TempDir()
				if mode == "explicit" {
					streams.ConfigPath = validPath
				}
				if mode == "default" {
					workingDirectory = validDirectory
				}
				withWorkingDirectory(t, workingDirectory, func() {
					if err := command.run(ctx, command.args, streams); err != nil {
						t.Fatalf("command failed under %s precedence: %v", mode, err)
					}
				})
			})
		}
	}
}

// TestCommandNonCreationLeavesNeverLoadPepper catches a composition mutation that loads key
// secret material for list/revoke, budget, or usage commands that do not hash credentials.
func TestCommandNonCreationLeavesNeverLoadPepper(t *testing.T) {
	dsn := commandStore(t)
	store := openCommandStore(t, dsn)
	ctx := context.Background()
	listKey, err := store.CreateKey(ctx, "pepperless", "list", "pepperless-list", make([]byte, 32), nil)
	if err != nil {
		t.Fatalf("seed list key: %v", err)
	}
	usageKey, err := store.CreateKey(ctx, "pepperless", "usage", "pepperless-usage", bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatalf("seed usage key: %v", err)
	}
	requestID := usageTestRequest(t, ctx, store, usageKey, time.Now().UTC().Add(-time.Hour))

	path := filepath.Join(t.TempDir(), "pepperless.yaml")
	writeCommandConfig(t, path)
	pepperReads := 0
	streams := Streams{Out: new(bytes.Buffer), Err: new(bytes.Buffer), ConfigPath: path, Getenv: func(name string) string {
		switch name {
		case "TEST_DSN":
			return dsn
		case "TEST_PEPPER":
			pepperReads++
			return ""
		}
		return ""
	}}
	run := func(label string, command func(context.Context, []string, Streams) error, args ...string) {
		t.Helper()
		streams.Out.(*bytes.Buffer).Reset()
		streams.Err.(*bytes.Buffer).Reset()
		if err := command(ctx, args, streams); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	run("key list", runKey, "list", "pepperless")
	run("key revoke", runKey, "revoke", strconv.FormatInt(listKey.ID, 10))
	run("budget set", runBudget, "set", "pepperless", "--dimension", "calls", "--window", "hour", "--max", "9", "--action", "block")
	limits, err := store.ListBudgets(ctx, "pepperless")
	if err != nil || len(limits) != 1 {
		t.Fatalf("seeded budgets = (%d, %v)", len(limits), err)
	}
	run("budget list", runBudget, "list", "pepperless")
	run("budget delete", runBudget, "delete", strconv.FormatInt(limits[0].ID, 10))
	run("usage show", runUsage, "show", "pepperless", "--since", "24h", "--by", "key")
	run("usage resolve", runUsage, "resolve", requestID, "--assume-zero")
	if pepperReads != 0 {
		t.Fatalf("non-creation commands loaded key pepper %d times", pepperReads)
	}
}

// TestCommandOutputErrorsAreNeverReportedAsSuccess catches mutations that discard writer errors
// from non-secret Task 9 output after a read or durable state transition.
func TestCommandOutputErrorsAreNeverReportedAsSuccess(t *testing.T) {
	dsn := commandStore(t)
	store := openCommandStore(t, dsn)
	ctx := context.Background()
	listKey, err := store.CreateKey(ctx, "output-errors", "list", "output-list", make([]byte, 32), nil)
	if err != nil {
		t.Fatalf("seed list key: %v", err)
	}
	usageKey, err := store.CreateKey(ctx, "output-errors", "usage", "output-usage", bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatalf("seed usage key: %v", err)
	}
	requestID := usageTestRequest(t, ctx, store, usageKey, time.Now().UTC().Add(-time.Hour))
	path := filepath.Join(t.TempDir(), "output-errors.yaml")
	writeCommandConfig(t, path)
	streams := Streams{Out: failingOutputWriter{}, Err: new(bytes.Buffer), ConfigPath: path, Getenv: func(name string) string {
		if name == "TEST_DSN" {
			return dsn
		}
		return ""
	}}
	assertFailure := func(label, contextText string, command func(context.Context, []string, Streams) error, args ...string) {
		t.Helper()
		err := command(ctx, args, streams)
		if err == nil || !errors.Is(err, errOutputDelivery) || !strings.Contains(err.Error(), contextText) {
			t.Fatalf("%s error = %v, want contextual output failure", label, err)
		}
	}
	assertFailure("key list", "write key list", runKey, "list", "output-errors")
	assertFailure("key revoke", "write revoked key", runKey, "revoke", strconv.FormatInt(listKey.ID, 10))
	assertFailure("budget set", "write budget", runBudget, "set", "output-errors", "--dimension", "calls", "--window", "hour", "--max", "3", "--action", "block")
	limits, err := store.ListBudgets(ctx, "output-errors")
	if err != nil || len(limits) != 1 {
		t.Fatalf("persisted output-error budget = (%d, %v)", len(limits), err)
	}
	assertFailure("budget list", "write budget list", runBudget, "list", "output-errors")
	assertFailure("budget delete", "write deleted budget", runBudget, "delete", strconv.FormatInt(limits[0].ID, 10))
	assertFailure("usage show", "write usage report", runUsage, "show", "output-errors", "--since", "24h", "--by", "key")
	assertFailure("usage resolve", "write usage resolution", runUsage, "resolve", requestID, "--assume-zero")
}

var errOutputDelivery = errors.New("output unavailable")

// failingOutputWriter rejects every success-path output write.
type failingOutputWriter struct{}

// Write implements io.Writer with one deterministic failure.
func (failingOutputWriter) Write([]byte) (int, error) { return 0, errOutputDelivery }

// TestCommandLeafParsersRejectFlagsBeforeOpeningStore catches a mutation that replaces any
// leaf FlagSet with raw positional parsing and reaches configuration or PostgreSQL on parser input.
func TestCommandLeafParsersRejectFlagsBeforeOpeningStore(t *testing.T) {
	tests := []struct {
		name    string
		run     func(context.Context, []string, Streams) error
		help    []string
		unknown []string
	}{
		{name: "key create", run: runKey, help: []string{"create", "--help"}, unknown: []string{"create", "--unknown"}},
		{name: "key list", run: runKey, help: []string{"list", "--help"}, unknown: []string{"list", "--unknown"}},
		{name: "key rotate", run: runKey, help: []string{"rotate", "--help"}, unknown: []string{"rotate", "--unknown"}},
		{name: "key revoke", run: runKey, help: []string{"revoke", "--help"}, unknown: []string{"revoke", "--unknown"}},
		{name: "budget set", run: runBudget, help: []string{"set", "--help"}, unknown: []string{"set", "--unknown"}},
		{name: "budget list", run: runBudget, help: []string{"list", "--help"}, unknown: []string{"list", "--unknown"}},
		{name: "budget delete", run: runBudget, help: []string{"delete", "--help"}, unknown: []string{"delete", "--unknown"}},
		{name: "usage show", run: runUsage, help: []string{"show", "--help"}, unknown: []string{"show", "--unknown"}},
		{name: "usage resolve", run: runUsage, help: []string{"resolve", "--help"}, unknown: []string{"resolve", "--unknown"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name string
				args []string
				help bool
			}{{name: "help", args: test.help, help: true}, {name: "unknown", args: test.unknown}} {
				t.Run(scenario.name, func(t *testing.T) {
					var errorsOutput bytes.Buffer
					streams := Streams{Out: new(bytes.Buffer), Err: &errorsOutput, ConfigPath: filepath.Join(t.TempDir(), "must-not-open.yaml"), Getenv: func(string) string { panic("parser reached configuration environment") }}
					err := test.run(context.Background(), scenario.args, streams)
					if scenario.help {
						if !errors.Is(err, flag.ErrHelp) {
							t.Fatalf("error = %v, want flag.ErrHelp (store must remain unopened)", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
						t.Fatalf("error = %v, want unknown-flag parser error (store must remain unopened)", err)
					}
					if errorsOutput.Len() == 0 {
						t.Fatal("parser wrote no help/error output to Streams.Err")
					}
				})
			}
		})
	}
}

// TestCommandUnknownNestedCommandsAreUsageErrors catches a parser mutation that accepts an
// unknown administrative subcommand or silently performs an unrelated operation.
func TestCommandUnknownNestedCommandsAreUsageErrors(t *testing.T) {
	for _, run := range []func(context.Context, []string, Streams) error{runKey, runBudget, runUsage} {
		var errOutput bytes.Buffer
		err := run(context.Background(), []string{"unknown"}, Streams{Err: &errOutput})
		if err == nil || !strings.Contains(err.Error(), "unknown") || errOutput.Len() == 0 {
			t.Fatalf("unknown nested command = (%v, %q), want usage error", err, errOutput.String())
		}
	}
}

// commandStore starts a real ephemeral PostgreSQL database and returns its connection string.
func commandStore(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	container, err := tcpostgres.Run(context.Background(), "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"), tcpostgres.WithUsername("llmgw"), tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(time.Minute)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(context.Background(), "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	return dsn
}

// commandStreams supplies controlled local I/O and environment to a leaf command.
func commandStreams(t *testing.T, dsn string) Streams {
	t.Helper()
	path := filepath.Join(t.TempDir(), "command.yaml")
	if err := os.WriteFile(path, []byte(commandConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Streams{
		In: os.Stdin, Out: new(bytes.Buffer), Err: new(bytes.Buffer), ConfigPath: path,
		Getenv: func(name string) string {
			if name == "TEST_DSN" {
				return dsn
			}
			if name == "TEST_PEPPER" {
				return strings.Repeat("p", 32)
			}
			return ""
		},
	}
}

// writeCommandConfig writes the shared local configuration at an exact test path.
func writeCommandConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(commandConfig), 0o600); err != nil {
		t.Fatalf("write command config: %v", err)
	}
}

// withWorkingDirectory scopes a process working-directory change to one synchronous callback.
func withWorkingDirectory(t *testing.T, directory string, run func()) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	}()
	run()
}

// testStreams supplies environment-only streams for configuration selection tests.
func testStreams(values map[string]string) Streams {
	return Streams{Getenv: func(name string) string { return values[name] }}
}

const commandConfig = `
host: 127.0.0.1
port: 8088
auth-dir: /tmp/auth
disable-image-generation: true
request-retry: 1
max-retry-credentials: 2
routing:
  strategy: round-robin
  session-affinity: false
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
llmgw:
  postgres-dsn-env: TEST_DSN
  key-pepper-env: TEST_PEPPER
`
