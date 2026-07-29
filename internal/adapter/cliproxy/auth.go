package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// AuthInfo is the intentionally non-secret projection of one local SDK auth file.
type AuthInfo struct {
	ID       string
	Provider string
	Label    string
	Disabled bool
}

// LegacyImport is the non-secret outcome of attempting one legacy credential export.
type LegacyImport struct {
	Provider string
	Label    string
	Status   string
}

// authManager is the public SDK manager boundary. It keeps OAuth/browser tests at the supported
// SDK surface rather than duplicating provider flows or reaching into upstream internals.
type authManager interface {
	Login(context.Context, string, *sdkconfig.Config, *sdkauth.LoginOptions) (*coreauth.Auth, string, error)
}

var newAuthManager = func(store coreauth.Store) authManager {
	return sdkauth.NewManager(
		store,
		sdkauth.NewCodexAuthenticator(),
		sdkauth.NewClaudeAuthenticator(),
		sdkauth.NewAntigravityAuthenticator(),
		sdkauth.NewKimiAuthenticator(),
		sdkauth.NewXAIAuthenticator(),
	)
}

// PrepareAuthDir creates one private auth root or verifies that an existing root is safe to use.
func PrepareAuthDir(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("prepare auth directory: path is required")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return errors.New("prepare auth directory: create failed")
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return errors.New("prepare auth directory: inspect failed")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("prepare auth directory: symbolic links are not allowed")
	}
	if !info.IsDir() {
		return errors.New("prepare auth directory: not a directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return errors.New("prepare auth directory: secure failed")
	}
	return nil
}

// ListAuth reads local SDK auth files and returns only safe, stable operator metadata.
func ListAuth(ctx context.Context, authDir string) ([]AuthInfo, error) {
	if err := PrepareAuthDir(authDir); err != nil {
		return nil, err
	}
	store, err := newSecureFileTokenStore(authDir)
	if err != nil {
		return nil, err
	}
	auths, err := store.List(ctx)
	if err != nil {
		return nil, errors.New("list local auth: unavailable")
	}
	infos := make([]AuthInfo, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		infos = append(infos, AuthInfo{ID: auth.ID, Provider: auth.Provider, Label: auth.Label, Disabled: auth.Disabled})
	}
	sort.Slice(infos, func(left, right int) bool {
		if infos[left].ID != infos[right].ID {
			return infos[left].ID < infos[right].ID
		}
		if infos[left].Provider != infos[right].Provider {
			return infos[left].Provider < infos[right].Provider
		}
		return infos[left].Label < infos[right].Label
	})
	return infos, nil
}

// Login executes a provider's public SDK authenticator and saves its result in the private auth directory.
func Login(ctx context.Context, cfg *sdkconfig.Config, provider string, options *sdkauth.LoginOptions) (AuthInfo, string, error) {
	if cfg == nil {
		return AuthInfo{}, "", errors.New("login provider: configuration is required")
	}
	if !supportedProvider(provider) {
		return AuthInfo{}, "", errors.New("login provider: unsupported provider")
	}
	if err := PrepareAuthDir(cfg.AuthDir); err != nil {
		return AuthInfo{}, "", fmt.Errorf("prepare provider auth directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return AuthInfo{}, "", err
	}
	store, err := newSecureFileTokenStore(cfg.AuthDir)
	if err != nil {
		return AuthInfo{}, "", err
	}
	manager := newAuthManager(store)
	auth, path, err := manager.Login(ctx, provider, cfg, options)
	if err != nil {
		return AuthInfo{}, "", errors.New("login provider: authentication failed")
	}
	if auth == nil {
		return AuthInfo{}, "", errors.New("login provider: invalid response")
	}
	return AuthInfo{ID: auth.ID, Provider: auth.Provider, Label: auth.Label, Disabled: auth.Disabled}, path, nil
}

// ImportLegacy exports only legacy formats that the public CLIProxyAPI file store understands.
func ImportLegacy(
	ctx context.Context,
	authDir string,
	credentials []governance.LegacyCredential,
) ([]LegacyImport, error) {
	if err := PrepareAuthDir(authDir); err != nil {
		return nil, err
	}
	absoluteAuthDir, err := filepath.Abs(authDir)
	if err != nil {
		return nil, errors.New("import legacy auth: resolve directory failed")
	}
	authDir = filepath.Clean(absoluteAuthDir)
	results := make([]LegacyImport, 0, len(credentials))
	for _, legacy := range credentials {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		provider, metadata, ok := legacyMetadata(legacy)
		result := LegacyImport{Provider: provider, Label: legacy.AccountLabel, Status: "needs-login"}
		if !ok {
			results = append(results, result)
			continue
		}
		base := sanitizeLegacyLabel(legacy.AccountLabel)
		if base == "" {
			results = append(results, result)
			continue
		}
		fileName := legacyFileName(provider, legacy.AccountLabel, base)
		path := filepath.Join(authDir, fileName)
		_, err := os.Lstat(path)
		if err == nil {
			result.Status = "exists"
			results = append(results, result)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("import legacy auth: inspect destination failed")
		}
		auth := &coreauth.Auth{ID: fileName, Provider: provider, FileName: fileName, Metadata: metadata}
		installed, err := stageAndInstallLegacyAuth(ctx, authDir, path, auth)
		if err != nil {
			return nil, err
		}
		if !installed {
			result.Status = "exists"
			results = append(results, result)
			continue
		}
		result.Status = "imported"
		results = append(results, result)
	}
	return results, nil
}

func stageAndInstallLegacyAuth(
	ctx context.Context,
	authDir string,
	finalPath string,
	auth *coreauth.Auth,
) (installed bool, returnErr error) {
	stageDir, err := os.MkdirTemp(authDir, ".llmgw-import-")
	if err != nil {
		return false, errors.New("import legacy auth: create stage failed")
	}
	defer func() {
		if cleanupErr := cleanupLegacyStage(stageDir, os.RemoveAll); cleanupErr != nil {
			if installed && returnErr == nil {
				returnErr = errors.New("import legacy auth: final installed; stage cleanup failed; manual cleanup required")
				return
			}
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err := os.Chmod(stageDir, 0o700); err != nil {
		return false, errors.New("import legacy auth: secure stage failed")
	}

	store, err := newSecureFileTokenStore(stageDir)
	if err != nil {
		return false, errors.New("import legacy auth: create stage store failed")
	}
	stagePath, err := store.Save(ctx, auth)
	if err != nil {
		return false, errors.New("import legacy auth: stage save failed")
	}
	expectedStagePath := filepath.Join(stageDir, auth.FileName)
	resolvedStagePath, err := filepath.Abs(stagePath)
	if err != nil || filepath.Clean(resolvedStagePath) != filepath.Clean(expectedStagePath) {
		return false, errors.New("import legacy auth: stage path escaped")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.Link(expectedStagePath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, errors.New("import legacy auth: atomic install failed")
	}
	if err := secureSavedAuthFile(finalPath); err != nil {
		verifyErr := errors.New("import legacy auth: installed file verification failed")
		if removalErr := removeLegacyInstalledFile(expectedStagePath, finalPath, removeInstalledLegacyAuth); removalErr != nil {
			return false, errors.Join(verifyErr, removalErr)
		}
		return false, verifyErr
	}
	return true, nil
}

func cleanupLegacyStage(stageDir string, remove func(string) error) error {
	removeErr := remove(stageDir)
	entries, inspectErr := os.ReadDir(stageDir)
	if errors.Is(inspectErr, os.ErrNotExist) {
		return nil
	}
	if inspectErr == nil && len(entries) == 0 {
		return nil
	}
	if removeErr == nil && inspectErr == nil {
		return errors.New("import legacy auth: stage cleanup incomplete; manual cleanup required")
	}
	return errors.New("import legacy auth: stage cleanup failed; manual cleanup required")
}

func removeLegacyInstalledFile(
	stagePath string,
	finalPath string,
	remove func(string, string) error,
) error {
	removeErr := remove(stagePath, finalPath)
	_, inspectErr := os.Lstat(finalPath)
	if errors.Is(inspectErr, os.ErrNotExist) {
		return nil
	}
	if removeErr == nil && inspectErr == nil {
		return errors.New("import legacy auth: installed file removal incomplete; manual cleanup required")
	}
	return errors.New("import legacy auth: installed file removal failed; manual cleanup required")
}

func removeInstalledLegacyAuth(stagePath, finalPath string) error {
	stageInfo, stageErr := os.Lstat(stagePath)
	finalInfo, finalErr := os.Lstat(finalPath)
	if stageErr != nil || finalErr != nil || !os.SameFile(stageInfo, finalInfo) {
		return errors.New("remove installed legacy auth: identity mismatch")
	}
	return os.Remove(finalPath)
}

func supportedProvider(provider string) bool {
	switch provider {
	case "claude", "codex", "antigravity", "kimi", "xai":
		return true
	default:
		return false
	}
}

func legacyMetadata(legacy governance.LegacyCredential) (string, map[string]any, bool) {
	label := strings.TrimSpace(legacy.AccountLabel)
	refresh := strings.TrimSpace(legacy.RefreshToken)
	switch legacy.Provider {
	case "claude_max_oauth":
		if label == "" || refresh == "" {
			return "claude", nil, false
		}
		metadata := map[string]any{"type": "claude", "email": legacy.AccountLabel, "access_token": legacy.AccessToken, "refresh_token": legacy.RefreshToken}
		if legacy.ExpiresAt != nil {
			metadata["expired"] = legacy.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return "claude", metadata, true
	case "chatgpt_codex_oauth":
		accountID := strings.TrimSpace(legacy.ChatGPTAccountID)
		if label == "" || refresh == "" || accountID == "" {
			return "codex", nil, false
		}
		metadata := map[string]any{"type": "codex", "email": legacy.AccountLabel, "access_token": legacy.AccessToken, "refresh_token": legacy.RefreshToken, "account_id": legacy.ChatGPTAccountID}
		if legacy.ExpiresAt != nil {
			metadata["expired"] = legacy.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return "codex", metadata, true
	default:
		return legacy.Provider, nil, false
	}
}

func sanitizeLegacyLabel(label string) string {
	var value strings.Builder
	lastDash := false
	for _, char := range strings.TrimSpace(strings.ToLower(label)) {
		switch {
		case unicode.IsLetter(char), unicode.IsDigit(char):
			value.WriteRune(char)
			lastDash = false
		case char == '.', char == '_', char == '-':
			value.WriteRune(char)
			lastDash = false
		default:
			if value.Len() > 0 && !lastDash {
				value.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(value.String(), ".-")
}

func legacyFileName(provider, exactLabel, safeLabel string) string {
	identity := sha256.Sum256([]byte(provider + "\x00" + exactLabel))
	return provider + "-legacy-" + safeLabel + "-" + hex.EncodeToString(identity[:6]) + ".json"
}
