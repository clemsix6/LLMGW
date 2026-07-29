package cliproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestSDKStartupSnapshotRejectsSymlinkRootWithoutDisclosure(t *testing.T) {
	target := t.TempDir()
	const secret = "hostile-root-secret"
	body := []byte(`{"type":"codex","access_token":"` + secret + `"}`)
	if err := os.WriteFile(filepath.Join(target, "credential.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "auth-link")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	cfg.Proxy.AuthDir = root

	snapshot, err := newSDKStartupSnapshot(cfg)

	if snapshot != nil || err == nil {
		t.Fatalf("snapshot symlink root = (%#v, %v), want nil, error", snapshot, err)
	}
	for _, disclosed := range []string{target, root, secret} {
		if strings.Contains(err.Error(), disclosed) {
			t.Fatal("snapshot error disclosed auth path or content")
		}
	}
}

func TestSDKStartupSnapshotRejectsSymlinkEntryWithoutDisclosure(t *testing.T) {
	fixture := newSDKFixture(t)
	outside := filepath.Join(t.TempDir(), "hostile-target.json")
	const secret = "hostile-entry-secret"
	body := []byte(`{"type":"codex","access_token":"` + secret + `"}`)
	if err := os.WriteFile(outside, body, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(fixture.authDir, "credential.json")
	if err := os.Symlink(outside, entry); err != nil {
		t.Fatal(err)
	}

	snapshot, err := newSDKStartupSnapshot(boundedServiceConfig(fixture))

	if snapshot != nil || err == nil {
		t.Fatalf("snapshot symlink entry = (%#v, %v), want nil, error", snapshot, err)
	}
	for _, disclosed := range []string{outside, entry, secret} {
		if strings.Contains(err.Error(), disclosed) {
			t.Fatal("snapshot error disclosed auth path or content")
		}
	}
}

func TestSDKStartupSnapshotRejectsSwapToSymlinkAtOpen(t *testing.T) {
	fixture := newSDKFixture(t)
	source := filepath.Join(fixture.authDir, "credential.json")
	outside := filepath.Join(t.TempDir(), "outside.json")
	const secret = "swapped-symlink-secret"
	if err := os.WriteFile(
		source,
		[]byte(`{"type":"codex","access_token":"`+secret+`"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, outside); err != nil {
		t.Fatal(err)
	}
	hooks := startupAuthSnapshotHooks{
		beforeFileOpen: func(name string) {
			if name != "credential.json" {
				return
			}
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, source); err != nil {
				t.Fatal(err)
			}
		},
	}

	snapshot, err := copyStartupAuthDirWithHooks(fixture.authDir, hooks)

	if snapshot != "" || err == nil {
		t.Fatalf("snapshot swapped symlink = (%q, %v), want empty/error", snapshot, err)
	}
	for _, disclosed := range []string{source, outside, secret} {
		if strings.Contains(err.Error(), disclosed) {
			t.Fatal("snapshot error disclosed auth path or content")
		}
	}
}

func TestSDKStartupSnapshotRejectsInPlaceMutationDuringRead(t *testing.T) {
	fixture := newSDKFixture(t)
	source := filepath.Join(fixture.authDir, "credential.json")
	const original = `{"type":"codex","token":"aaaa"}`
	const mutated = `{"type":"codex","token":"bbbb"}`
	if len(original) != len(mutated) {
		t.Fatal("test payloads must have equal size")
	}
	if err := os.WriteFile(source, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	hooks := startupAuthSnapshotHooks{
		beforeSecondRead: func(name string) {
			if name != "credential.json" {
				return
			}
			if err := os.WriteFile(source, []byte(mutated), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
	}

	snapshot, err := copyStartupAuthDirWithHooks(fixture.authDir, hooks)

	if snapshot != "" || err == nil {
		t.Fatalf("snapshot in-place mutation = (%q, %v), want empty/error", snapshot, err)
	}
	if strings.Contains(err.Error(), source) ||
		strings.Contains(err.Error(), original) ||
		strings.Contains(err.Error(), mutated) {
		t.Fatal("snapshot mutation error disclosed auth path or content")
	}
}

func TestSDKStartupSnapshotRequiresFlatJSONEntries(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		fixture := newSDKFixture(t)
		if err := os.Mkdir(filepath.Join(fixture.authDir, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}

		snapshot, err := newSDKStartupSnapshot(boundedServiceConfig(fixture))

		if snapshot != nil || err == nil {
			t.Fatalf("snapshot directory entry = (%#v, %v), want nil/error", snapshot, err)
		}
	})

	t.Run("non JSON regular file", func(t *testing.T) {
		fixture := newSDKFixture(t)
		if err := os.WriteFile(
			filepath.Join(fixture.authDir, "credential.txt"),
			[]byte(`{"type":"codex"}`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}

		snapshot, err := newSDKStartupSnapshot(boundedServiceConfig(fixture))

		if snapshot != nil || err == nil {
			t.Fatalf("snapshot non-JSON entry = (%#v, %v), want nil/error", snapshot, err)
		}
	})
}

func TestSDKStartupSnapshotFreezesConfigAuthAndPermissions(t *testing.T) {
	fixture := newSDKFixture(t)
	sourcePath := filepath.Join(fixture.authDir, "credential.json")
	const originalAuth = `{"type":"codex","access_token":"initial"}`
	if err := os.WriteFile(sourcePath, []byte(originalAuth), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := boundedServiceConfig(fixture)
	enabled := true
	cfg.Proxy.AntigravitySignatureCacheEnabled = &enabled
	cfg.Proxy.OAuthExcludedModels = map[string][]string{"codex": {"model-a"}}
	cfg.Proxy.OpenAICompatibility = []sdkconfig.OpenAICompatibility{{
		Name:    "pool",
		Headers: map[string]string{"X-Test": "initial"},
		Models: []sdkconfig.OpenAICompatibilityModel{{
			Name:            "upstream-a",
			Alias:           "shared",
			InputModalities: []string{"text"},
		}},
	}}

	snapshot, err := newSDKStartupSnapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshotDir := snapshot.config.Proxy.AuthDir
	snapshotPath := filepath.Join(snapshotDir, "credential.json")

	cfg.Proxy.RequestRetry = 200
	*cfg.Proxy.AntigravitySignatureCacheEnabled = false
	cfg.Proxy.OAuthExcludedModels["codex"][0] = "mutated"
	cfg.Proxy.OpenAICompatibility[0].Headers["X-Test"] = "mutated"
	cfg.Proxy.OpenAICompatibility[0].Models[0].Name = "mutated"
	cfg.Proxy.OpenAICompatibility[0].Models[0].InputModalities[0] = "mutated"
	if err := os.WriteFile(sourcePath, []byte(`{"type":"codex","access_token":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if snapshot.config.Proxy == cfg.Proxy ||
		snapshot.config.Proxy.RequestRetry != 0 ||
		snapshot.config.Proxy.AntigravitySignatureCacheEnabled == cfg.Proxy.AntigravitySignatureCacheEnabled ||
		!*snapshot.config.Proxy.AntigravitySignatureCacheEnabled ||
		snapshot.config.Proxy.OAuthExcludedModels["codex"][0] != "model-a" ||
		snapshot.config.Proxy.OpenAICompatibility[0].Headers["X-Test"] != "initial" ||
		snapshot.config.Proxy.OpenAICompatibility[0].Models[0].Name != "upstream-a" ||
		snapshot.config.Proxy.OpenAICompatibility[0].Models[0].InputModalities[0] != "text" {
		t.Fatal("startup configuration shares mutable state with caller")
	}
	gotAuth, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAuth) != originalAuth {
		t.Fatal("startup auth snapshot changed with source")
	}
	assertPrivateMode(t, snapshotDir, 0o700)
	assertPrivateMode(t, snapshotPath, 0o600)

	if err := snapshot.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory remains after cleanup: %v", err)
	}
}

func TestSDKStartupSnapshotBoundsAuthFilesAndAllowsMissingDirectory(t *testing.T) {
	t.Run("file size", func(t *testing.T) {
		fixture := newSDKFixture(t)
		body := make([]byte, 1<<20+1)
		if err := os.WriteFile(filepath.Join(fixture.authDir, "large.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := newSDKStartupSnapshot(boundedServiceConfig(fixture))
		if snapshot != nil || err == nil || strings.Contains(err.Error(), fixture.authDir) {
			t.Fatalf("oversize snapshot = (%#v, %v), want safe error", snapshot, err)
		}
	})

	t.Run("file count", func(t *testing.T) {
		fixture := newSDKFixture(t)
		for index := 0; index < maximumStartupAuthFiles; index++ {
			name := filepath.Join(fixture.authDir, deterministicRequestID(index)+".json")
			if err := os.WriteFile(name, []byte(`{"type":"codex"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(
			filepath.Join(fixture.authDir, "ignored-before-round-4.txt"),
			[]byte("not JSON"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		snapshot, err := newSDKStartupSnapshot(boundedServiceConfig(fixture))
		if snapshot != nil || err == nil {
			t.Fatalf("too-many snapshot = (%#v, %v), want nil, error", snapshot, err)
		}
	})

	t.Run("total size", func(t *testing.T) {
		fixture := newSDKFixture(t)
		body := make([]byte, 1<<20)
		copy(body, `{"type":"codex"}`)
		for index := len(`{"type":"codex"}`); index < len(body); index++ {
			body[index] = ' '
		}
		for index := 0; index < 17; index++ {
			name := filepath.Join(fixture.authDir, deterministicRequestID(index)+".json")
			if err := os.WriteFile(name, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := newSDKStartupSnapshot(boundedServiceConfig(fixture))
		if snapshot != nil || err == nil {
			t.Fatalf("oversize-total snapshot = (%#v, %v), want nil, error", snapshot, err)
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		fixture := newSDKFixture(t)
		cfg := boundedServiceConfig(fixture)
		cfg.Proxy.AuthDir = filepath.Join(t.TempDir(), "missing")
		snapshot, err := newSDKStartupSnapshot(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := snapshot.Cleanup(); err != nil {
				t.Error(err)
			}
		}()
		entries, err := os.ReadDir(snapshot.config.Proxy.AuthDir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("missing source snapshot entries/error = %d/%v, want 0/nil", len(entries), err)
		}
	})
}

func TestSDKStartupSnapshotValidatesTildeAuthRetryAfterCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(authDir, "credential.json"),
		[]byte(`{"type":"codex","metadata":{"request_retry":200}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	fixture := newSDKFixture(t)
	cfg := boundedServiceConfig(fixture)
	cfg.Proxy.AuthDir = "~/auth"

	snapshot, err := newSDKStartupSnapshot(cfg)

	if snapshot != nil || err == nil ||
		!strings.Contains(err.Error(), "auth retry override exceeds request-retry") {
		t.Fatalf("snapshot hostile tilde auth = (%#v, %v), want nil/rejection",
			snapshot, err)
	}
}

func assertPrivateMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode = %04o, want %04o", got, want)
	}
}
