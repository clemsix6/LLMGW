package command

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestRootDispatch catches a root mutation that drops the implicit serve command or sends a
// top-level command to the wrong leaf.
func TestRootDispatch(t *testing.T) {
	original := commandHandlers
	t.Cleanup(func() { commandHandlers = original })

	var calls []string
	commandHandlers = map[string]commandHandler{}
	for _, name := range []string{"serve", "auth", "key", "budget", "usage"} {
		name := name
		commandHandlers[name] = func(_ context.Context, args []string, _ Streams) error {
			calls = append(calls, name+":"+strings.Join(args, ","))
			return nil
		}
	}

	for _, args := range [][]string{
		nil,
		{"serve"},
		{"auth", "list"},
		{"key", "list"},
		{"budget", "list"},
		{"usage", "show"},
	} {
		if err := Run(context.Background(), args, testRootStreams(nil)); err != nil {
			t.Fatalf("Run(%v): %v", args, err)
		}
	}
	want := []string{"serve:", "serve:", "auth:list", "key:list", "budget:list", "usage:show"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("dispatch calls = %#v, want %#v", calls, want)
	}
}

// TestRootConfigPrecedence catches a mutation that resolves the config after dispatch or lets the
// environment override an explicit global flag for any command family.
func TestRootConfigPrecedence(t *testing.T) {
	original := commandHandlers
	t.Cleanup(func() { commandHandlers = original })

	commandHandlers = map[string]commandHandler{}
	for _, name := range []string{"serve", "auth", "key", "budget", "usage"} {
		name := name
		commandHandlers[name] = func(_ context.Context, _ []string, streams Streams) error {
			if streams.ConfigPath != "/explicit.yaml" {
				return errors.New(name + " received wrong config path: " + streams.ConfigPath)
			}
			return nil
		}
	}
	for name := range commandHandlers {
		if err := Run(
			context.Background(),
			[]string{"--config", "/explicit.yaml", name},
			testRootStreams(map[string]string{"LLMGW_CONFIG": "/environment.yaml"}),
		); err != nil {
			t.Fatalf("%s explicit config: %v", name, err)
		}
	}

	commandHandlers["serve"] = func(_ context.Context, _ []string, streams Streams) error {
		if streams.ConfigPath != "/environment.yaml" {
			return errors.New("environment config path was not resolved")
		}
		return nil
	}
	if err := Run(context.Background(), nil, testRootStreams(map[string]string{
		"LLMGW_CONFIG": "/environment.yaml",
	})); err != nil {
		t.Fatalf("environment config: %v", err)
	}
}

// TestRootUsage catches acceptance of global flags after a command, unknown commands, and help
// output that omits an operator-facing command family.
func TestRootUsage(t *testing.T) {
	streams := testRootStreams(nil)
	if err := Run(context.Background(), []string{"unknown"}, streams); err == nil {
		t.Fatal("unknown command succeeded")
	}

	streams = testRootStreams(nil)
	if err := Run(context.Background(), []string{"--help"}, streams); err != nil {
		t.Fatalf("--help: %v", err)
	}
	help := streams.Out.(*bytes.Buffer).String()
	for _, command := range []string{"serve", "auth", "key", "budget", "usage"} {
		if !strings.Contains(help, command) {
			t.Fatalf("help %q omits %q", help, command)
		}
	}
}

// TestRootRejectsLateConfigWithoutDependencies catches a leaf that opens configuration or another
// command dependency before rejecting a global flag placed after the command.
func TestRootRejectsLateConfigWithoutDependencies(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "serve", args: []string{"serve", "--config", "/late.yaml"}, wantErr: "serve accepts no arguments"},
		{name: "auth", args: []string{"auth", "list", "--config", "/late.yaml"}, wantErr: "flag provided but not defined: -config"},
		{name: "key", args: []string{"key", "list", "--config", "/late.yaml"}, wantErr: "flag provided but not defined: -config"},
		{name: "budget", args: []string{"budget", "list", "--config", "/late.yaml"}, wantErr: "flag provided but not defined: -config"},
		{name: "usage", args: []string{"usage", "show", "--config", "/late.yaml"}, wantErr: "flag provided but not defined: -config"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			streams := testRootStreams(nil)
			streams.ConfigPath = "/configuration-must-not-open.yaml"
			err := Run(context.Background(), test.args, streams)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Run(%v) error = %v, want %q", test.args, err, test.wantErr)
			}
		})
	}
}

func testRootStreams(environment map[string]string) Streams {
	return Streams{
		In:  strings.NewReader(""),
		Out: new(bytes.Buffer),
		Err: new(bytes.Buffer),
		Getenv: func(name string) string {
			return environment[name]
		},
	}
}
