package command

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clemsix6/LLMGW/internal/adapter/cliproxy"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestAuthCommandListsLocalCredentialsWithoutDatabaseOrPepper catches a composition mutation
// that opens PostgreSQL or resolves the key pepper for an auth-directory-only listing.
func TestAuthCommandListsLocalCredentialsWithoutDatabaseOrPepper(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude.json"), []byte(`{"type":"claude","email":"operator@example.test","access_token":"access-secret","refresh_token":"refresh-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeAuthCommandConfig(t, dir)
	var out, errOut bytes.Buffer
	err := runAuth(context.Background(), []string{"list"}, Streams{Out: &out, Err: &errOut, ConfigPath: path, Getenv: func(name string) string {
		if name == "TEST_DSN" || name == "TEST_PEPPER" {
			panic("auth list resolved a database or pepper secret")
		}
		return ""
	}})
	if err != nil {
		t.Fatalf("auth list: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "id\tclaude.json") || !strings.Contains(got, "provider\tclaude") || !strings.Contains(got, "label\toperator@example.test") || !strings.Contains(got, "disabled\tfalse") || strings.Contains(got, "access-secret") || strings.Contains(got, "refresh-secret") {
		t.Fatalf("auth list output = %q", got)
	}
}

// TestAuthCommandRejectsUnsupportedLoginBeforeOAuth catches a whitelist mutation that starts an
// arbitrary SDK authenticator or reads the configuration secrets on invalid provider input.
func TestAuthCommandRejectsUnsupportedLoginBeforeOAuth(t *testing.T) {
	var errorOutput bytes.Buffer
	err := runAuth(context.Background(), []string{"login", "unsupported"}, Streams{Err: &errorOutput, ConfigPath: writeAuthCommandConfig(t, t.TempDir()), Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "auth login requires") || errorOutput.Len() == 0 {
		t.Fatalf("unsupported login = (%v, %q), want usage failure", err, errorOutput.String())
	}
}

// TestAuthCommandOutputFailureIsVisible catches a command mutation that completes a safe local
// listing while silently dropping its operator-visible output.
func TestAuthCommandOutputFailureIsVisible(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude.json"), []byte(`{"type":"claude","email":"operator@example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runAuth(context.Background(), []string{"list"}, Streams{Out: failingOutputWriter{}, Err: new(bytes.Buffer), ConfigPath: writeAuthCommandConfig(t, dir), Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "write auth list") {
		t.Fatalf("output failure = %v, want contextual error", err)
	}
}

// TestAuthCommandPassesDeviceOptionsAndManualPrompt catches a parser mutation that drops the
// Codex device marker, callback port, no-browser setting, or Streams-backed manual callback I/O.
func TestAuthCommandPassesDeviceOptionsAndManualPrompt(t *testing.T) {
	dir := t.TempDir()
	previous := authLogin
	var gotProvider string
	authLogin = func(_ context.Context, _ *sdkconfig.Config, provider string, options *sdkauth.LoginOptions) (cliproxy.AuthInfo, string, error) {
		gotProvider = provider
		if !options.NoBrowser || options.CallbackPort != 1717 || options.Metadata["codex_login_mode"] != "device" {
			t.Fatalf("login options = %#v", options)
		}
		value, err := options.Prompt("paste callback: ")
		if err != nil || value != "https://callback.example.test/?code=ok" {
			t.Fatalf("prompt = (%q, %v)", value, err)
		}
		return cliproxy.AuthInfo{Provider: "codex", Label: "operator@example.test"}, filepath.Join(dir, "codex.json"), nil
	}
	t.Cleanup(func() { authLogin = previous })
	var out, errOut bytes.Buffer
	err := runAuth(context.Background(), []string{"login", "--device", "--no-browser", "--callback-port", "1717", "codex"}, Streams{In: strings.NewReader("https://callback.example.test/?code=ok\n"), Out: &out, Err: &errOut, ConfigPath: writeAuthCommandConfig(t, dir), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if gotProvider != "codex" || !strings.Contains(out.String(), "path\t") || !strings.Contains(out.String(), "provider\tcodex") || !strings.Contains(errOut.String(), "paste callback: ") {
		t.Fatalf("login channels = provider %q stdout %q stderr %q", gotProvider, out.String(), errOut.String())
	}
}

// TestAuthCommandRejectsLateConfigBeforeDependencies catches auth loading YAML or preparing its
// auth directory before its selected leaf rejects a misplaced global flag.
func TestAuthCommandRejectsLateConfigBeforeDependencies(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runAuth(
		context.Background(),
		[]string{"list", "--config", "/late.yaml"},
		Streams{
			In:         strings.NewReader(""),
			Out:        &out,
			Err:        &errOut,
			ConfigPath: filepath.Join(t.TempDir(), "configuration-must-not-open.yaml"),
			Getenv: func(string) string {
				panic("auth late-flag validation read the environment")
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -config") {
		t.Fatalf("late auth config error = %v, want leaf flag usage error", err)
	}
}

// TestAuthCommandImportsLegacyWithoutSecretOutput catches an import mutation that loads the
// pepper, alters source rows, or writes refresh/account/session secrets to operator channels.
func TestAuthCommandImportsLegacyWithoutSecretOutput(t *testing.T) {
	dsn := commandStore(t)
	store := openCommandStore(t, dsn)
	ctx := context.Background()
	seedPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open credential seed pool: %v", err)
	}
	t.Cleanup(seedPool.Close)
	const insert = `
INSERT INTO oauth_token (provider_id, account_label, access_token, refresh_token, chatgpt_account_id)
VALUES ((SELECT id FROM provider WHERE type = $1), $2, $3, $4, $5)`
	if _, err := seedPool.Exec(ctx, insert, "claude_max_oauth", "claude-label", "claude-access-secret", "claude-refresh-secret", nil); err != nil {
		t.Fatalf("seed claude credential: %v", err)
	}
	if _, err := seedPool.Exec(ctx, insert, "chatgpt_codex_oauth", "codex-label", nil, "codex-refresh-secret", "codex-account-secret"); err != nil {
		t.Fatalf("seed codex credential: %v", err)
	}
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	err = runAuth(ctx, []string{"import-legacy"}, Streams{Out: &out, Err: &errOut, ConfigPath: writeAuthCommandConfig(t, dir), Getenv: func(name string) string {
		if name == "TEST_DSN" {
			return dsn
		}
		if name == "TEST_PEPPER" {
			panic("auth import loaded key pepper")
		}
		return ""
	}})
	if err != nil {
		t.Fatalf("auth import: %v", err)
	}
	combined := out.String() + errOut.String()
	for _, secret := range []string{"claude-access-secret", "claude-refresh-secret", "codex-refresh-secret", "codex-account-secret"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("import output leaked %q: %q", secret, combined)
		}
	}
	if !strings.Contains(out.String(), "status\timported") {
		t.Fatalf("import output = %q", out.String())
	}
	credentials, err := store.LegacyCredentials(ctx)
	if err != nil || len(credentials) != 2 || credentials[0].RefreshToken == "" || credentials[1].RefreshToken == "" {
		t.Fatalf("source credentials after import = %#v, %v", credentials, err)
	}
}

func writeAuthCommandConfig(t *testing.T, authDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth-command.yaml")
	data := strings.Replace(commandConfig, "auth-dir: /tmp/auth", "auth-dir: "+authDir, 1)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write auth config: %v", err)
	}
	return path
}
