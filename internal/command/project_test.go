package command

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestProjectUsageAndArguments proves every malformed project invocation is
// rejected before the command ever opens a store — asserted by checking the
// error never carries openStore's "load command configuration" wrapper, given
// a ConfigPath that only that call would ever read — and that each failure
// prints the shared usage line.
func TestProjectUsageAndArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command", nil},
		{"unknown command", []string{"delete"}},
		{"list with extra argument", []string{"list", "extra"}},
		{"tool-prefix missing state", []string{"tool-prefix", "demo"}},
		{"tool-prefix missing name and state", []string{"tool-prefix"}},
		{"tool-prefix invalid state", []string{"tool-prefix", "demo", "maybe"}},
		{"tool-prefix extra argument", []string{"tool-prefix", "demo", "on", "extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, errOut := new(bytes.Buffer), new(bytes.Buffer)
			streams := Streams{
				Out:        out,
				Err:        errOut,
				ConfigPath: "/nonexistent/llmgw-project-test-config.yaml",
				Getenv:     func(string) string { return "" },
			}
			err := runProject(context.Background(), test.args, streams)
			if err == nil {
				t.Fatal("malformed project command succeeded")
			}
			if strings.Contains(err.Error(), "load command configuration") {
				t.Fatalf("validation reached the store: %v", err)
			}
			if !strings.Contains(errOut.String(), "usage: project {list|tool-prefix}") {
				t.Fatalf("usage output = %q, want the shared usage line", errOut.String())
			}
		})
	}
}

// TestProjectListAndToolPrefix proves list and tool-prefix read and write
// through the real store: the list shape carries name, creation time, and
// prefix state; tool-prefix flips the flag both ways; an unknown project is
// rejected rather than created.
func TestProjectListAndToolPrefix(t *testing.T) {
	factory := newCommandStreamsFactory(t)
	seedProject(t, factory, "prefix-demo")

	assertProjectListState(t, factory, "prefix-demo", false)

	runProjectOrFail(t, factory, "tool-prefix", "prefix-demo", "on")
	assertProjectListState(t, factory, "prefix-demo", true)

	runProjectOrFail(t, factory, "tool-prefix", "prefix-demo", "off")
	assertProjectListState(t, factory, "prefix-demo", false)

	streams, _, _ := factory.streams()
	err := runProject(context.Background(), []string{"tool-prefix", "does-not-exist", "on"}, streams)
	if err == nil || !strings.Contains(err.Error(), `"does-not-exist" does not exist`) {
		t.Fatalf("unknown project error = %v, want a does-not-exist message", err)
	}
}

// commandStreamsFactory builds fresh Streams sharing one configuration and
// environment, so each command invocation gets its own output buffers.
type commandStreamsFactory struct {
	configPath string
	getenv     func(string) string
}

// streams returns a ready Streams together with its output and error buffers.
func (f commandStreamsFactory) streams() (Streams, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	return Streams{Out: out, Err: errOut, ConfigPath: f.configPath, Getenv: f.getenv}, out, errOut
}

// newCommandStreamsFactory starts an ephemeral PostgreSQL 16 instance and
// writes the minimal valid shared configuration pointing at it, skipping the
// test outright when no container provider is available.
func newCommandStreamsFactory(t *testing.T) commandStreamsFactory {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	dsn := startCommandPostgres(t)
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatalf("create command test auth dir: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	config := fmt.Sprintf(`
auth-dir: %q
disable-image-generation: true
request-retry: 0
max-retry-credentials: 1
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
home:
  enabled: false
pprof:
  enable: false
routing:
  session-affinity: false
llmgw:
  postgres-dsn-env: TEST_PROJECT_COMMAND_DSN
  key-pepper-env: TEST_PROJECT_COMMAND_PEPPER
  usage-retention-days: 35
  usage-outstanding-capacity: 2
`, authDir)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write command test config: %v", err)
	}

	return commandStreamsFactory{
		configPath: configPath,
		getenv: func(name string) string {
			switch name {
			case "TEST_PROJECT_COMMAND_DSN":
				return dsn
			case "TEST_PROJECT_COMMAND_PEPPER":
				return "command-test-key-pepper-32-bytes!!"
			default:
				return ""
			}
		},
	}
}

// startCommandPostgres starts an ephemeral PostgreSQL 16 instance and returns its DSN.
func startCommandPostgres(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("llmgw"),
		tcpostgres.WithUsername("llmgw"),
		tcpostgres.WithPassword("llmgw"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start command postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate command postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("command postgres connection string: %v", err)
	}
	return dsn
}

// seedProject creates one project through the same key-creation path an
// operator uses, since implicit project creation stays a property of key
// create alone.
func seedProject(t *testing.T, factory commandStreamsFactory, name string) {
	t.Helper()
	streams, _, _ := factory.streams()
	cfg, store, err := openStore(context.Background(), streams.ConfigPath, streams)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	defer store.Close()
	service, err := keyService(cfg, store, streams)
	if err != nil {
		t.Fatalf("build seed key service: %v", err)
	}
	if _, err := service.Create(context.Background(), name, "seed", nil); err != nil {
		t.Fatalf("seed project %q: %v", name, err)
	}
}

// runProjectOrFail runs one project command that must succeed and returns its output.
func runProjectOrFail(t *testing.T, factory commandStreamsFactory, args ...string) string {
	t.Helper()
	streams, out, _ := factory.streams()
	if err := runProject(context.Background(), args, streams); err != nil {
		t.Fatalf("run project %v: %v", args, err)
	}
	return out.String()
}

// assertProjectListState runs project list and checks one project's three
// printed fields: its name, a created_at value, and the expected prefix state.
func assertProjectListState(t *testing.T, factory commandStreamsFactory, name string, prefixEnabled bool) {
	t.Helper()
	output := runProjectOrFail(t, factory, "list")
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if line != "name\t"+name {
			continue
		}
		if i+2 >= len(lines) {
			t.Fatalf("project %q entry is truncated: %q", name, output)
		}
		if !strings.HasPrefix(lines[i+1], "created_at\t") {
			t.Fatalf("project %q missing created_at: %q", name, output)
		}
		want := fmt.Sprintf("prefix_tool_names\t%t", prefixEnabled)
		if lines[i+2] != want {
			t.Fatalf("project %q prefix state = %q, want %q", name, lines[i+2], want)
		}
		return
	}
	t.Fatalf("project list = %q, missing entry for %q", output, name)
}
