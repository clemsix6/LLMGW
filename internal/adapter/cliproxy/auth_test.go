package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestAuthListPrintsOnlySafeLocalMetadata catches a listing mutation that exposes persisted
// token metadata instead of the documented operator-facing identity fields.
func TestAuthListPrintsOnlySafeLocalMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-safe.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","email":"operator@example.test","disabled":true,"access_token":"access-secret","refresh_token":"refresh-secret","account_id":"account-secret","session_key":"session-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	auths, err := ListAuth(context.Background(), dir)

	if err != nil {
		t.Fatalf("list auth: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("listed auths = %d, want 1", len(auths))
	}
	if got, want := auths[0], (AuthInfo{ID: "codex-safe.json", Provider: "codex", Label: "operator@example.test", Disabled: true}); got != want {
		t.Fatalf("auth = %#v, want %#v", got, want)
	}
}

// TestAuthListRejectsSymlinkEntryWithoutReadingOutside catches a listing mutation that follows a
// JSON symlink and projects metadata from outside the prepared local auth directory.
func TestAuthListRejectsSymlinkEntryWithoutReadingOutside(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	const outsideLabel = "outside-sensitive-label"
	if err := os.WriteFile(outside, []byte(`{"type":"claude","email":"`+outsideLabel+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}

	auths, err := ListAuth(context.Background(), dir)

	if err == nil || len(auths) != 0 {
		t.Fatalf("symlink list = (%#v, %v), want no records/error", auths, err)
	}
	if strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), outsideLabel) {
		t.Fatalf("list error disclosed outside auth data: %v", err)
	}
}

// TestAuthListRejectsHardLinkedEntryWithoutReadingOutside catches a listing mutation that treats
// a multiply-linked outside inode as an independently owned local auth file.
func TestAuthListRejectsHardLinkedEntryWithoutReadingOutside(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	const outsideLabel = "outside-hard-link-label"
	if err := os.WriteFile(outside, []byte(`{"type":"claude","email":"`+outsideLabel+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}

	auths, err := ListAuth(context.Background(), dir)

	if err == nil || len(auths) != 0 {
		t.Fatalf("hard-link list = (%#v, %v), want no records/error", auths, err)
	}
	if strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), outsideLabel) {
		t.Fatalf("list error disclosed outside hard-link data: %v", err)
	}
}

// TestImportLegacyWritesOnlyCompatibleCredentials catches mutations that export session keys,
// omit required Codex account identifiers, or persist a non-private auth directory or file.
func TestImportLegacyWritesOnlyCompatibleCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	expires := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	results, err := ImportLegacy(context.Background(), dir, []governance.LegacyCredential{
		{Provider: "claude_max_oauth", AccountLabel: "Claude Account", AccessToken: "claude-access-secret", RefreshToken: "claude-refresh-secret", SessionKey: "claude-session-secret", ExpiresAt: &expires},
		{Provider: "chatgpt_codex_oauth", AccountLabel: "Codex Account", AccessToken: "codex-access-secret", RefreshToken: "codex-refresh-secret", ChatGPTAccountID: "codex-account-secret"},
		{Provider: "claude_max_oauth", AccountLabel: "requires login", SessionKey: "only-session-key"},
		{Provider: "unsupported", AccountLabel: "unknown", RefreshToken: "unknown-refresh"},
	})

	if err != nil {
		t.Fatalf("import legacy: %v", err)
	}
	if got, want := results, []LegacyImport{
		{Provider: "claude", Label: "Claude Account", Status: "imported"},
		{Provider: "codex", Label: "Codex Account", Status: "imported"},
		{Provider: "claude", Label: "requires login", Status: "needs-login"},
		{Provider: "unsupported", Label: "unknown", Status: "needs-login"},
	}; !sameLegacyImports(got, want) {
		t.Fatalf("import results = %#v, want %#v", got, want)
	}
	assertAuthMode(t, dir, 0o700)

	claudePath := filepath.Join(dir, "claude-legacy-claude-account-affc5c9cf726.json")
	claude := readAuthJSON(t, claudePath)
	if claude["type"] != "claude" || claude["email"] != "Claude Account" || claude["access_token"] != "claude-access-secret" || claude["refresh_token"] != "claude-refresh-secret" || claude["expired"] != expires.Format(time.RFC3339) {
		t.Fatalf("claude auth = %#v", claude)
	}
	if _, found := claude["session_key"]; found {
		t.Fatalf("claude auth leaked session key: %#v", claude)
	}
	assertAuthMode(t, claudePath, 0o600)

	codexPath := filepath.Join(dir, "codex-legacy-codex-account-f607fabb592c.json")
	codex := readAuthJSON(t, codexPath)
	if codex["type"] != "codex" || codex["account_id"] != "codex-account-secret" || codex["refresh_token"] != "codex-refresh-secret" {
		t.Fatalf("codex auth = %#v", codex)
	}
	assertAuthMode(t, codexPath, 0o600)
}

// TestImportLegacyDoesNotOverwriteExistingFile catches a retry mutation that replaces an
// operator-managed credential after the initial import.
func TestImportLegacyDoesNotOverwriteExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-legacy-existing-85f4edb0ba5a.json")
	original := []byte(`{"type":"claude","refresh_token":"operator-owned"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := ImportLegacy(context.Background(), dir, []governance.LegacyCredential{{
		Provider: "claude_max_oauth", AccountLabel: "existing", RefreshToken: "legacy-refresh-secret",
	}})
	if err != nil {
		t.Fatalf("import legacy: %v", err)
	}
	if got, want := results, []LegacyImport{{Provider: "claude", Label: "existing", Status: "exists"}}; !sameLegacyImports(got, want) {
		t.Fatalf("results = %#v, want %#v", got, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("existing auth = %q, want untouched %q", got, original)
	}
}

// TestImportLegacyDoesNotOverwriteExistingSymlink proves the no-overwrite install treats a
// pre-existing symlink as occupied and never follows it to an outside credential target.
func TestImportLegacyDoesNotOverwriteExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	original := []byte(`{"outside":"operator-owned"}`)
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claude-legacy-symlinked-47e6a100926b.json")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}

	results, err := ImportLegacy(context.Background(), dir, []governance.LegacyCredential{{
		Provider: "claude_max_oauth", AccountLabel: "symlinked", RefreshToken: "legacy-refresh-secret",
	}})

	if err != nil {
		t.Fatalf("import with existing symlink: %v", err)
	}
	if got, want := results, []LegacyImport{{Provider: "claude", Label: "symlinked", Status: "exists"}}; !sameLegacyImports(got, want) {
		t.Fatalf("symlink results = %#v, want %#v", got, want)
	}
	linkInfo, err := os.Lstat(path)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("existing symlink changed: %v, %v", linkInfo, err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("existing symlink target = %q, want untouched %q", got, original)
	}
}

// TestImportLegacyDisambiguatesSanitizedLabelCollisions catches a filename mutation that maps
// distinct source rows such as "A B" and "a-b" onto one destination and drops a credential.
func TestImportLegacyDisambiguatesSanitizedLabelCollisions(t *testing.T) {
	dir := t.TempDir()
	credentials := []governance.LegacyCredential{
		{Provider: "claude_max_oauth", AccountLabel: "A B", AccessToken: "first-access-secret", RefreshToken: "first-refresh-secret"},
		{Provider: "claude_max_oauth", AccountLabel: "a-b", AccessToken: "second-access-secret", RefreshToken: "second-refresh-secret"},
	}

	first, err := ImportLegacy(context.Background(), dir, credentials)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if got, want := first, []LegacyImport{
		{Provider: "claude", Label: "A B", Status: "imported"},
		{Provider: "claude", Label: "a-b", Status: "imported"},
	}; !sameLegacyImports(got, want) {
		t.Fatalf("first collision results = %#v, want %#v", got, want)
	}
	firstNames := legacyJSONNames(t, dir)
	if len(firstNames) != 2 || firstNames[0] == firstNames[1] {
		t.Fatalf("collision filenames = %#v, want two distinct files", firstNames)
	}
	for _, name := range firstNames {
		if !strings.HasPrefix(name, "claude-legacy-a-b-") || !strings.HasSuffix(name, ".json") {
			t.Fatalf("collision filename lost safe recognizable prefix: %q", name)
		}
		for _, secret := range []string{"first-access-secret", "first-refresh-secret", "second-access-secret", "second-refresh-secret"} {
			if strings.Contains(name, secret) {
				t.Fatalf("collision filename leaked credential: %q", name)
			}
		}
	}

	second, err := ImportLegacy(context.Background(), dir, credentials)
	if err != nil {
		t.Fatalf("retry import: %v", err)
	}
	if got, want := second, []LegacyImport{
		{Provider: "claude", Label: "A B", Status: "exists"},
		{Provider: "claude", Label: "a-b", Status: "exists"},
	}; !sameLegacyImports(got, want) {
		t.Fatalf("retry collision results = %#v, want %#v", got, want)
	}
	secondNames := legacyJSONNames(t, dir)
	if strings.Join(firstNames, "\x00") != strings.Join(secondNames, "\x00") {
		t.Fatalf("retry filenames changed: first %#v second %#v", firstNames, secondNames)
	}
}

var (
	errInjectedStageSave = errors.New("injected stage save failure")
	errInjectedInstall   = errors.New("injected install failure")
	errInjectedCleanup   = errors.New("injected cleanup failure")
	errInjectedVerify    = errors.New("injected final verification failure")
	errInjectedRemoval   = errors.New("injected installed-file removal failure")
)

// TestImportLegacyFailureLeavesNoFinalAndRetrySucceeds catches a persistence mutation that writes
// the final path directly or leaves a failed staging artifact that makes the next import skip.
func TestImportLegacyFailureLeavesNoFinalAndRetrySucceeds(t *testing.T) {
	credential := governance.LegacyCredential{
		Provider: "claude_max_oauth", AccountLabel: "retryable", AccessToken: "access-secret", RefreshToken: "refresh-secret",
	}
	tests := []struct {
		name  string
		hooks legacyImportHooks
	}{
		{
			name: "partial stage save",
			hooks: legacyImportHooks{newStageStore: func(dir string) (legacyStageStore, error) {
				return partialFailureStore{dir: dir}, nil
			}},
		},
		{
			name: "atomic install",
			hooks: legacyImportHooks{install: func(string, string) error {
				return errInjectedInstall
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()

			_, err := importLegacyWithHooks(context.Background(), dir, []governance.LegacyCredential{credential}, test.hooks)

			if err == nil {
				t.Fatal("injected import failure succeeded")
			}
			for _, secret := range []string{"access-secret", "refresh-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("import error leaked %q: %v", secret, err)
				}
			}
			assertNoLegacyArtifacts(t, dir)

			results, err := ImportLegacy(context.Background(), dir, []governance.LegacyCredential{credential})
			if err != nil {
				t.Fatalf("retry after failure: %v", err)
			}
			if got, want := results, []LegacyImport{{Provider: "claude", Label: "retryable", Status: "imported"}}; !sameLegacyImports(got, want) {
				t.Fatalf("retry results = %#v, want %#v", got, want)
			}
			names := legacyJSONNames(t, dir)
			if len(names) != 1 {
				t.Fatalf("retry auth files = %#v, want one", names)
			}
			assertAuthMode(t, filepath.Join(dir, names[0]), 0o600)
			assertNoStagingDirectories(t, dir)
		})
	}
}

// TestImportLegacySurfacesCombinedCleanupFailures catches cleanup mutations that discard a
// secondary error or imply rollback/retry safety while secret stage/final artifacts remain.
func TestImportLegacySurfacesCombinedCleanupFailures(t *testing.T) {
	credential := governance.LegacyCredential{
		Provider: "claude_max_oauth", AccountLabel: "cleanup", AccessToken: "cleanup-access-secret", RefreshToken: "cleanup-refresh-secret",
	}
	assertRedacted := func(t *testing.T, err error) {
		t.Helper()
		for _, secret := range []string{"cleanup-access-secret", "cleanup-refresh-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("cleanup error leaked %q: %v", secret, err)
			}
		}
	}

	t.Run("stage cleanup combined with save error", func(t *testing.T) {
		dir := t.TempDir()
		hooks := legacyImportHooks{
			newStageStore: func(stageDir string) (legacyStageStore, error) {
				return partialFailureStore{dir: stageDir}, nil
			},
			removeStage: func(string) error { return errInjectedCleanup },
		}

		_, err := importLegacyWithHooks(context.Background(), dir, []governance.LegacyCredential{credential}, hooks)

		if err == nil || !strings.Contains(err.Error(), "stage save failed") ||
			!strings.Contains(err.Error(), "stage cleanup failed") ||
			!strings.Contains(err.Error(), "manual cleanup required") {
			t.Fatalf("combined save/cleanup error = %v", err)
		}
		assertRedacted(t, err)
		stages := legacyStageDirectories(t, dir)
		if len(stages) != 1 {
			t.Fatalf("retained failed stages = %#v, want one explicit recoverable artifact", stages)
		}
		if err := os.RemoveAll(filepath.Join(dir, stages[0])); err != nil {
			t.Fatal(err)
		}
		assertLegacyRetryImports(t, dir, credential)
	})

	t.Run("stage cleanup combined with install error", func(t *testing.T) {
		dir := t.TempDir()
		hooks := legacyImportHooks{
			install:     func(string, string) error { return errInjectedInstall },
			removeStage: func(string) error { return errInjectedCleanup },
		}

		_, err := importLegacyWithHooks(context.Background(), dir, []governance.LegacyCredential{credential}, hooks)

		if err == nil || !strings.Contains(err.Error(), "atomic install failed") ||
			!strings.Contains(err.Error(), "stage cleanup failed") ||
			!strings.Contains(err.Error(), "manual cleanup required") {
			t.Fatalf("combined install/cleanup error = %v", err)
		}
		assertRedacted(t, err)
		stages := legacyStageDirectories(t, dir)
		if len(stages) != 1 {
			t.Fatalf("retained failed install stages = %#v, want one explicit recoverable artifact", stages)
		}
		if err := os.RemoveAll(filepath.Join(dir, stages[0])); err != nil {
			t.Fatal(err)
		}
		assertLegacyRetryImports(t, dir, credential)
	})

	t.Run("verification and installed removal failures", func(t *testing.T) {
		dir := t.TempDir()
		hooks := legacyImportHooks{
			verifyInstalled: func(string) error { return errInjectedVerify },
			removeInstalled: func(string, string) error { return errInjectedRemoval },
		}

		_, err := importLegacyWithHooks(context.Background(), dir, []governance.LegacyCredential{credential}, hooks)

		if err == nil || !strings.Contains(err.Error(), "installed file verification failed") ||
			!strings.Contains(err.Error(), "installed file removal failed") ||
			!strings.Contains(err.Error(), "manual cleanup required") {
			t.Fatalf("combined verify/removal error = %v", err)
		}
		assertRedacted(t, err)
		names := legacyJSONNames(t, dir)
		if len(names) != 1 {
			t.Fatalf("retained unverifiable finals = %#v, want one explicit recoverable artifact", names)
		}
		if stages := legacyStageDirectories(t, dir); len(stages) != 0 {
			t.Fatalf("verification failure left stage directories: %#v", stages)
		}
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			t.Fatal(err)
		}
		assertLegacyRetryImports(t, dir, credential)
	})

	t.Run("cleanup failure after successful install", func(t *testing.T) {
		dir := t.TempDir()
		hooks := legacyImportHooks{
			removeStage: func(string) error { return errInjectedCleanup },
		}

		_, err := importLegacyWithHooks(context.Background(), dir, []governance.LegacyCredential{credential}, hooks)

		if err == nil || !strings.Contains(err.Error(), "final installed") ||
			!strings.Contains(err.Error(), "stage cleanup failed") ||
			!strings.Contains(err.Error(), "manual cleanup required") {
			t.Fatalf("post-install cleanup error = %v", err)
		}
		assertRedacted(t, err)
		names := legacyJSONNames(t, dir)
		stages := legacyStageDirectories(t, dir)
		if len(names) != 1 || len(stages) != 1 {
			t.Fatalf("post-install explicit state = finals %#v stages %#v, want one each", names, stages)
		}
		if err := os.RemoveAll(filepath.Join(dir, stages[0])); err != nil {
			t.Fatal(err)
		}
		results, retryErr := ImportLegacy(context.Background(), dir, []governance.LegacyCredential{credential})
		if retryErr != nil || len(results) != 1 || results[0].Status != "exists" {
			t.Fatalf("retry after manual stage cleanup = (%#v, %v), want existing final", results, retryErr)
		}
	})

	t.Run("empty stage cleanup failure keeps verified success", func(t *testing.T) {
		dir := t.TempDir()
		hooks := legacyImportHooks{
			install:     os.Rename,
			removeStage: func(string) error { return errInjectedCleanup },
		}

		results, err := importLegacyWithHooks(context.Background(), dir, []governance.LegacyCredential{credential}, hooks)

		if err != nil || len(results) != 1 || results[0].Status != "imported" {
			t.Fatalf("empty-stage cleanup result = (%#v, %v), want successful import", results, err)
		}
		names := legacyJSONNames(t, dir)
		stages := legacyStageDirectories(t, dir)
		if len(names) != 1 || len(stages) != 1 {
			t.Fatalf("empty-stage explicit state = finals %#v stages %#v, want one each", names, stages)
		}
		stageEntries, readErr := os.ReadDir(filepath.Join(dir, stages[0]))
		if readErr != nil || len(stageEntries) != 0 {
			t.Fatalf("failed cleanup stage contains secret artifacts: %v, %v", stageEntries, readErr)
		}
	})
}

// TestImportLegacyStagesRelativeAuthDirectoryWithinThatRoot catches an atomic-stage mutation that
// compares an absolute public-store result to a relative destination and rejects valid config.
func TestImportLegacyStagesRelativeAuthDirectoryWithinThatRoot(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	working := t.TempDir()
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	results, err := ImportLegacy(context.Background(), "auth", []governance.LegacyCredential{{
		Provider: "claude_max_oauth", AccountLabel: "relative", RefreshToken: "refresh-secret",
	}})

	if err != nil {
		t.Fatalf("relative auth import: %v", err)
	}
	if len(results) != 1 || results[0].Status != "imported" {
		t.Fatalf("relative auth results = %#v", results)
	}
	names := legacyJSONNames(t, filepath.Join(working, "auth"))
	if len(names) != 1 {
		t.Fatalf("relative auth files = %#v, want one", names)
	}
}

// TestPrepareAuthDirRejectsUnsafeRoots catches a directory-preparation mutation that follows a
// symbolic link, accepts a file, or leaves a private auth root group-readable.
func TestPrepareAuthDirRejectsUnsafeRoots(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if err := PrepareAuthDir(""); err == nil {
			t.Fatal("empty auth directory succeeded")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "auth-link")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		if err := PrepareAuthDir(link); err == nil {
			t.Fatal("symlink auth directory succeeded")
		}
	})
	t.Run("enforces private mode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "auth")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := PrepareAuthDir(dir); err != nil {
			t.Fatalf("prepare auth dir: %v", err)
		}
		assertAuthMode(t, dir, 0o700)
	})
}

func readAuthJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth %s: %v", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode auth %s: %v", path, err)
	}
	return value
}

func assertAuthMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}

func sameLegacyImports(got, want []LegacyImport) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func legacyJSONNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func assertNoLegacyArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed import artifacts = %v, want empty auth dir", entries)
	}
}

func assertNoStagingDirectories(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("staging directory remained: %q", entry.Name())
		}
	}
}

func legacyStageDirectories(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".llmgw-import-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func assertLegacyRetryImports(t *testing.T, dir string, credential governance.LegacyCredential) {
	t.Helper()
	results, err := ImportLegacy(context.Background(), dir, []governance.LegacyCredential{credential})
	if err != nil || len(results) != 1 || results[0].Status != "imported" {
		t.Fatalf("retry import = (%#v, %v), want imported", results, err)
	}
}

type partialFailureStore struct {
	dir string
}

func (s partialFailureStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	path := filepath.Join(s.dir, auth.FileName)
	if err := os.WriteFile(path, []byte(`{\"partial\":\"secret\"}`), 0o644); err != nil {
		return "", err
	}
	return "", errInjectedStageSave
}

func TestLegacyImportResultsNeverContainSecrets(t *testing.T) {
	results, err := ImportLegacy(context.Background(), t.TempDir(), []governance.LegacyCredential{{
		Provider: "claude_max_oauth", AccountLabel: "safe-label", AccessToken: "access-secret", RefreshToken: "refresh-secret", SessionKey: "session-secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{results[0].Provider, results[0].Label, results[0].Status} {
		for _, secret := range []string{"access-secret", "refresh-secret", "session-secret"} {
			if strings.Contains(value, secret) {
				t.Fatalf("result leaked %q: %#v", secret, results)
			}
		}
	}
}

// TestLoginUsesOnlyThePublicAuthenticatorManager catches a login mutation that skips private
// directory preparation or changes the public manager options passed to the SDK boundary.
func TestLoginUsesOnlyThePublicAuthenticatorManager(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	manager := &recordingAuthManager{auth: &coreauth.Auth{ID: "codex.json", Provider: "codex", Label: "operator@example.test"}, path: filepath.Join(dir, "codex.json")}
	previous := newAuthManager
	newAuthManager = func(coreauth.Store) authManager { return manager }
	t.Cleanup(func() { newAuthManager = previous })
	options := &sdkauth.LoginOptions{NoBrowser: true, CallbackPort: 1717, Metadata: map[string]string{"codex_login_mode": "device"}}

	info, path, err := Login(context.Background(), &sdkconfig.Config{AuthDir: dir}, "codex", options)

	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if info.Provider != "codex" || info.Label != "operator@example.test" || path != filepath.Join(dir, "codex.json") {
		t.Fatalf("login result = (%#v, %q)", info, path)
	}
	if manager.provider != "codex" || manager.options != options {
		t.Fatalf("manager call = provider %q options %#v", manager.provider, manager.options)
	}
	assertAuthMode(t, dir, 0o700)
}

// TestLoginRejectsUnexpectedNilPublicRecord catches a boundary mutation that turns an invalid
// public authenticator response into a process panic instead of a safe command error.
func TestLoginRejectsUnexpectedNilPublicRecord(t *testing.T) {
	previous := newAuthManager
	newAuthManager = func(coreauth.Store) authManager { return &recordingAuthManager{} }
	t.Cleanup(func() { newAuthManager = previous })

	_, _, err := Login(context.Background(), &sdkconfig.Config{AuthDir: t.TempDir()}, "claude", nil)

	if err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("nil public auth = %v, want safe error", err)
	}
}

// TestLoginTokenStoreEnforcesPrivateRegularFile catches a public-store mutation that accepts an
// authenticator storage implementation leaving a successful credential group/world-readable.
func TestLoginTokenStoreEnforcesPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	store, err := newSecureFileTokenStore(dir)
	if err != nil {
		t.Fatalf("create secure token store: %v", err)
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: &coreauth.Auth{
		ID:       "claude-operator.json",
		Provider: "claude",
		FileName: "claude-operator.json",
		Storage:  permissiveTokenStorage{payload: []byte(`{"type":"claude","refresh_token":"secret"}`)},
	}})

	_, savedPath, err := manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: dir}, nil)

	if err != nil {
		t.Fatalf("public manager login save: %v", err)
	}
	if savedPath != filepath.Join(dir, "claude-operator.json") {
		t.Fatalf("saved path = %q", savedPath)
	}
	info, err := os.Lstat(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved login mode = %v, want regular 0600", info.Mode())
	}
}

// TestLoginTokenStoreRejectsEscapingAuthenticatorFilenames catches a boundary mutation that lets
// a provider-derived filename or path attribute escape the already-prepared auth directory.
func TestLoginTokenStoreRejectsEscapingAuthenticatorFilenames(t *testing.T) {
	tests := []struct {
		name       string
		fileName   func(root, authDir string) string
		attributes func(root string) map[string]string
		target     func(root, authDir string) string
	}{
		{
			name:     "absolute",
			fileName: func(root, _ string) string { return filepath.Join(root, "absolute.json") },
			target:   func(root, _ string) string { return filepath.Join(root, "absolute.json") },
		},
		{
			name:     "traversal",
			fileName: func(_, _ string) string { return "../traversal.json" },
			target:   func(root, _ string) string { return filepath.Join(root, "traversal.json") },
		},
		{
			name:     "separator",
			fileName: func(_, _ string) string { return "nested/credential.json" },
			target:   func(_, authDir string) string { return filepath.Join(authDir, "nested", "credential.json") },
		},
		{
			name:     "portable separator",
			fileName: func(_, _ string) string { return `nested\credential.json` },
			target:   func(_, authDir string) string { return filepath.Join(authDir, `nested\credential.json`) },
		},
		{
			name:     "empty",
			fileName: func(_, _ string) string { return "" },
			target:   func(_, authDir string) string { return filepath.Join(authDir, "fallback-id.json") },
		},
		{
			name:     "path attribute",
			fileName: func(_, _ string) string { return "safe.json" },
			attributes: func(root string) map[string]string {
				return map[string]string{coreauth.AttributePath: filepath.Join(root, "attribute.json")}
			},
			target: func(root, _ string) string { return filepath.Join(root, "attribute.json") },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			authDir := filepath.Join(root, "auth")
			if err := PrepareAuthDir(authDir); err != nil {
				t.Fatal(err)
			}
			store, err := newSecureFileTokenStore(authDir)
			if err != nil {
				t.Fatal(err)
			}
			record := &coreauth.Auth{
				ID:       "fallback-id.json",
				Provider: "claude",
				FileName: test.fileName(root, authDir),
				Metadata: map[string]any{"type": "claude", "refresh_token": "secret"},
			}
			if test.attributes != nil {
				record.Attributes = test.attributes(root)
			}
			manager := sdkauth.NewManager(store, staticAuthenticator{record: record})

			_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

			if err == nil {
				t.Fatal("unsafe public authenticator filename succeeded")
			}
			if _, statErr := os.Lstat(test.target(root, authDir)); !os.IsNotExist(statErr) {
				t.Fatalf("unsafe target was created: %v", statErr)
			}
		})
	}
}

// TestLoginTokenStoreRejectsExistingDestinationSymlinkBeforeWrite catches a no-follow mutation
// that validates only after the public store has already truncated a symlink target.
func TestLoginTokenStoreRejectsExistingDestinationSymlinkBeforeWrite(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := PrepareAuthDir(authDir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	original := []byte(`{"outside":"untouched"}`)
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(authDir, "claude.json")); err != nil {
		t.Fatal(err)
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: &coreauth.Auth{
		ID: "claude.json", Provider: "claude", FileName: "claude.json",
		Metadata: map[string]any{"type": "claude", "refresh_token": "new-secret"},
	}})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err == nil {
		t.Fatal("existing destination symlink succeeded")
	}
	got, readErr := os.ReadFile(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("outside symlink target changed: got %q want %q", got, original)
	}
}

// TestLoginTokenStoreReplacesHardLinkWithoutMutatingOutsideAlias catches a staged-save mutation
// that still delegates directly to an existing destination inode before replacing its entry.
func TestLoginTokenStoreReplacesHardLinkWithoutMutatingOutsideAlias(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := PrepareAuthDir(authDir); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	destination := filepath.Join(authDir, "claude.json")
	original := []byte(`{"outside":"must-remain-byte-identical"}`)
	replacement := []byte(`{"type":"claude","refresh_token":"replacement-secret"}`)
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, destination); err != nil {
		t.Fatal(err)
	}
	outsideBefore, err := os.Lstat(outside)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: &coreauth.Auth{
		ID: "claude.json", Provider: "claude", FileName: "claude.json",
		Storage: permissiveTokenStorage{payload: replacement},
	}})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err != nil {
		t.Fatalf("replace hard-linked login: %v", err)
	}
	gotOutside, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOutside) != string(original) {
		t.Fatalf("outside hard-link alias changed: got %q want %q", gotOutside, original)
	}
	gotDestination, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDestination) != string(replacement) {
		t.Fatalf("replacement credential = %q, want %q", gotDestination, replacement)
	}
	destinationInfo, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(outsideBefore, destinationInfo) {
		t.Fatal("replacement destination still aliases outside inode")
	}
	if !authFileHasSingleLink(destinationInfo) {
		t.Fatal("replacement destination does not have exactly one link")
	}
	assertAuthMode(t, destination, 0o600)
}

// TestLoginTokenStoreFailurePreservesPriorCredential catches a direct-save mutation that truncates
// a valid destination before the public authenticator storage reports failure.
func TestLoginTokenStoreFailurePreservesPriorCredential(t *testing.T) {
	authDir := t.TempDir()
	destination := filepath.Join(authDir, "claude.json")
	original := []byte(`{"type":"claude","refresh_token":"prior-secret"}`)
	if err := os.WriteFile(destination, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: &coreauth.Auth{
		ID: "claude.json", Provider: "claude", FileName: "claude.json",
		Storage: writeThenFailTokenStorage{payload: []byte(`{"partial":"new-secret"}`)},
	}})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err == nil {
		t.Fatal("failing public storage reported login success")
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("prior credential changed after save failure: got %q want %q", got, original)
	}
}

// TestLoginTokenStoreSurfacesSaveAndCleanupFailures catches a stage-cleanup mutation that
// discards RemoveAll failure after the public SDK store has left partial credential bytes.
func TestLoginTokenStoreSurfacesSaveAndCleanupFailures(t *testing.T) {
	authDir := t.TempDir()
	const secret = `{"partial":"login-stage-save-secret"}`
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	store.removeStage = func(string) error { return errInjectedCleanup }
	record := &coreauth.Auth{
		ID:         "claude.json",
		Provider:   "claude",
		FileName:   "claude.json",
		Attributes: map[string]string{"custom": "preserved"},
		Storage:    writeThenFailTokenStorage{payload: []byte(secret)},
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: record})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err == nil || !strings.Contains(err.Error(), "stage save failed") ||
		!strings.Contains(err.Error(), "stage cleanup failed") ||
		!strings.Contains(err.Error(), "manual cleanup required") {
		t.Fatalf("save/cleanup error = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "login-stage-save-secret") {
		t.Fatalf("save/cleanup error disclosed credential content: %v", err)
	}
	assertOriginalLoginAttributes(t, record)
	stages := loginStageDirectories(t, authDir)
	if len(stages) != 1 {
		t.Fatalf("retained login stages = %#v, want one explicit recovery stage", stages)
	}
	stageEntries, readErr := os.ReadDir(filepath.Join(authDir, stages[0]))
	if readErr != nil || len(stageEntries) != 1 {
		t.Fatalf("retained login stage entries = %v, %v", stageEntries, readErr)
	}
	residual, readErr := os.ReadFile(filepath.Join(authDir, stages[0], stageEntries[0].Name()))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(residual) != secret {
		t.Fatal("retained login stage did not contain the partial credential requiring manual cleanup")
	}

	if err := os.RemoveAll(filepath.Join(authDir, stages[0])); err != nil {
		t.Fatal(err)
	}
	store.removeStage = os.RemoveAll
	record.Storage = permissiveTokenStorage{payload: []byte(`{"type":"claude","refresh_token":"retry-secret"}`)}
	_, path, err := manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)
	if err != nil {
		t.Fatalf("retry login after explicit cleanup: %v", err)
	}
	assertSuccessfulLoginAttributes(t, record, filepath.Join(authDir, "claude.json"), path)
	if stages := loginStageDirectories(t, authDir); len(stages) != 0 {
		t.Fatalf("successful retry left login stages: %#v", stages)
	}
}

// TestLoginTokenStoreSurfacesVerificationAndCleanupFailures catches a stage-verification
// mutation that returns a sensitive verifier error and discards the failed stage removal.
func TestLoginTokenStoreSurfacesVerificationAndCleanupFailures(t *testing.T) {
	authDir := t.TempDir()
	const secret = "login-stage-verification-secret"
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	store.verifyStage = func(string) error { return errors.New("injected verifier: " + secret) }
	store.removeStage = func(string) error { return errInjectedCleanup }
	record := &coreauth.Auth{
		ID:         "claude.json",
		Provider:   "claude",
		FileName:   "claude.json",
		Attributes: map[string]string{"custom": "preserved"},
		Storage:    permissiveTokenStorage{payload: []byte(`{"refresh_token":"` + secret + `"}`)},
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: record})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err == nil || !strings.Contains(err.Error(), "stage verification failed") ||
		!strings.Contains(err.Error(), "stage cleanup failed") ||
		!strings.Contains(err.Error(), "manual cleanup required") {
		t.Fatalf("verification/cleanup error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("verification/cleanup error disclosed credential content: %v", err)
	}
	assertOriginalLoginAttributes(t, record)
	stages := loginStageDirectories(t, authDir)
	if len(stages) != 1 {
		t.Fatalf("retained verification stages = %#v, want one explicit recovery stage", stages)
	}
	if err := os.RemoveAll(filepath.Join(authDir, stages[0])); err != nil {
		t.Fatal(err)
	}
	store.verifyStage = secureSavedAuthFile
	store.removeStage = os.RemoveAll
	_, path, err := manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)
	if err != nil {
		t.Fatalf("retry login after verification-stage cleanup: %v", err)
	}
	assertSuccessfulLoginAttributes(t, record, filepath.Join(authDir, "claude.json"), path)
}

// TestLoginTokenStoreRejectsMultiplyLinkedStageBeforeCommit catches moving the staged-inode
// link-count check after Rename, where failure would overwrite the prior credential.
func TestLoginTokenStoreRejectsMultiplyLinkedStageBeforeCommit(t *testing.T) {
	authDir := t.TempDir()
	destination := filepath.Join(authDir, "claude.json")
	original := []byte(`{"type":"claude","refresh_token":"prior-secret"}`)
	const replacementSecret = "precommit-link-secret"
	if err := os.WriteFile(destination, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	store.verifyStage = func(path string) error {
		if err := secureSavedAuthFile(path); err != nil {
			return err
		}
		return os.Link(path, path+".alias")
	}
	store.removeStage = func(string) error { return errInjectedCleanup }
	replaceCalls := 0
	store.replace = func(_, _ string) error {
		replaceCalls++
		return nil
	}
	record := &coreauth.Auth{
		ID:         "claude.json",
		Provider:   "claude",
		FileName:   "claude.json",
		Attributes: map[string]string{"custom": "preserved"},
		Storage:    permissiveTokenStorage{payload: []byte(`{"refresh_token":"` + replacementSecret + `"}`)},
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: record})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err == nil || !strings.Contains(err.Error(), "staged credential must have one link") ||
		!strings.Contains(err.Error(), "stage cleanup failed") ||
		!strings.Contains(err.Error(), "manual cleanup required") {
		t.Fatalf("precommit link/cleanup error = %v", err)
	}
	if strings.Contains(err.Error(), replacementSecret) {
		t.Fatalf("precommit link/cleanup error disclosed credential content: %v", err)
	}
	if replaceCalls != 0 {
		t.Fatalf("atomic replace calls = %d, want zero before staged inode validation", replaceCalls)
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("prior credential changed before commit: got %q want %q", got, original)
	}
	assertOriginalLoginAttributes(t, record)
	stages := loginStageDirectories(t, authDir)
	if len(stages) != 1 {
		t.Fatalf("retained multiply-linked stages = %#v, want one explicit recovery stage", stages)
	}

	if err := os.RemoveAll(filepath.Join(authDir, stages[0])); err != nil {
		t.Fatal(err)
	}
	store.verifyStage = secureSavedAuthFile
	store.removeStage = os.RemoveAll
	store.replace = os.Rename
	_, path, err := manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)
	if err != nil {
		t.Fatalf("retry login after linked-stage cleanup: %v", err)
	}
	assertSuccessfulLoginAttributes(t, record, destination, path)
	replaced, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(replaced), "prior-secret") {
		t.Fatal("successful retry retained prior credential bytes")
	}
}

// TestLoginTokenStoreCommittedReplaceIgnoresEmptyStageCleanupFailure catches removing the
// committed-state branch and turning a successful replacement into an error after commit.
func TestLoginTokenStoreCommittedReplaceIgnoresEmptyStageCleanupFailure(t *testing.T) {
	authDir := t.TempDir()
	destination := filepath.Join(authDir, "claude.json")
	original := []byte(`{"type":"claude","refresh_token":"prior-secret"}`)
	replacement := []byte(`{"type":"claude","refresh_token":"replacement-secret"}`)
	if err := os.WriteFile(destination, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	var retainedStage string
	var cleanupSawCommitted bool
	store.removeStage = func(stageDir string) error {
		retainedStage = stageDir
		data, readErr := os.ReadFile(destination)
		cleanupSawCommitted = readErr == nil && string(data) == string(replacement)
		entries, readErr := os.ReadDir(stageDir)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("post-commit stage = %v, %v, want empty directory", entries, readErr)
		}
		if chmodErr := os.Chmod(stageDir, 0); chmodErr != nil {
			t.Fatal(chmodErr)
		}
		return errInjectedCleanup
	}
	record := &coreauth.Auth{
		ID:         "claude.json",
		Provider:   "claude",
		FileName:   "claude.json",
		Attributes: map[string]string{"custom": "preserved"},
		Storage:    permissiveTokenStorage{payload: replacement},
	}
	manager := sdkauth.NewManager(store, staticAuthenticator{record: record})

	_, path, err := manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if retainedStage != "" {
		t.Cleanup(func() {
			_ = os.Chmod(retainedStage, 0o700)
			_ = os.RemoveAll(retainedStage)
		})
	}
	if err != nil {
		t.Fatalf("committed login reported cleanup failure: %v", err)
	}
	if !cleanupSawCommitted {
		t.Fatal("stage cleanup did not run after the replacement committed")
	}
	assertSuccessfulLoginAttributes(t, record, destination, path)
	got, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(replacement) || string(got) == string(original) {
		t.Fatalf("committed credential = %q, want replacement", got)
	}
	info, statErr := os.Lstat(destination)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		!authFileHasSingleLink(info) {
		t.Fatalf("committed credential mode/links = %v, want regular 0600 single-link", info.Mode())
	}
	stageInfo, statErr := os.Lstat(retainedStage)
	if statErr != nil || !stageInfo.IsDir() {
		t.Fatalf("retained stage = %v, %v, want empty private directory", stageInfo, statErr)
	}
}

// TestLoginTokenStoreReplaceFailurePreservesPriorCredential catches an atomic-replace mutation
// that removes or mutates the old entry before the directory-entry replacement succeeds.
func TestLoginTokenStoreReplaceFailurePreservesPriorCredential(t *testing.T) {
	authDir := t.TempDir()
	destination := filepath.Join(authDir, "claude.json")
	original := []byte(`{"type":"claude","refresh_token":"prior-secret"}`)
	if err := os.WriteFile(destination, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	store.replace = func(string, string) error { return errors.New("injected replace failure") }
	manager := sdkauth.NewManager(store, staticAuthenticator{record: &coreauth.Auth{
		ID: "claude.json", Provider: "claude", FileName: "claude.json",
		Storage: permissiveTokenStorage{payload: []byte(`{"type":"claude","refresh_token":"replacement-secret"}`)},
	}})

	_, _, err = manager.Login(context.Background(), "claude", &sdkconfig.Config{AuthDir: authDir}, nil)

	if err == nil {
		t.Fatal("failing atomic replacement reported login success")
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("prior credential changed after replace failure: got %q want %q", got, original)
	}
	entries, readDirErr := os.ReadDir(authDir)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	if len(entries) != 1 || entries[0].Name() != "claude.json" {
		t.Fatalf("replace failure left login stage artifacts: %v", entries)
	}
}

func loginStageDirectories(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".llmgw-login-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func assertOriginalLoginAttributes(t *testing.T, auth *coreauth.Auth) {
	t.Helper()
	if len(auth.Attributes) != 1 || auth.Attributes["custom"] != "preserved" {
		t.Fatalf("failed login attributes = %#v, want only original custom attribute", auth.Attributes)
	}
}

func assertSuccessfulLoginAttributes(t *testing.T, auth *coreauth.Auth, expectedPath, returnedPath string) {
	t.Helper()
	if returnedPath != expectedPath ||
		auth.Attributes["custom"] != "preserved" ||
		auth.Attributes[coreauth.AttributePath] != expectedPath ||
		auth.Attributes[coreauth.AttributeSource] != expectedPath ||
		auth.Attributes[coreauth.AttributeSourceBackend] != coreauth.AuthSourceFile {
		t.Fatalf("successful login path/attributes = (%q, %#v), want final path %q", returnedPath, auth.Attributes, expectedPath)
	}
}

type staticAuthenticator struct {
	record *coreauth.Auth
}

func (staticAuthenticator) Provider() string            { return "claude" }
func (staticAuthenticator) RefreshLead() *time.Duration { return nil }
func (a staticAuthenticator) Login(context.Context, *sdkconfig.Config, *sdkauth.LoginOptions) (*coreauth.Auth, error) {
	return a.record, nil
}

type permissiveTokenStorage struct {
	payload []byte
}

type writeThenFailTokenStorage struct {
	payload []byte
}

func (s writeThenFailTokenStorage) SaveTokenToFile(path string) error {
	if err := os.WriteFile(path, s.payload, 0o644); err != nil {
		return err
	}
	return errors.New("injected token storage failure")
}

func (s permissiveTokenStorage) SaveTokenToFile(path string) error {
	if err := os.WriteFile(path, s.payload, 0o644); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

type recordingAuthManager struct {
	auth     *coreauth.Auth
	path     string
	provider string
	options  *sdkauth.LoginOptions
}

func (m *recordingAuthManager) Login(_ context.Context, provider string, _ *sdkconfig.Config, options *sdkauth.LoginOptions) (*coreauth.Auth, string, error) {
	m.provider, m.options = provider, options
	return m.auth, m.path, nil
}
