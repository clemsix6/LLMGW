package cliproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// secureFileTokenStore contains the public SDK file store and verifies each successful
// credential write before reporting it to the authentication manager.
type secureFileTokenStore struct {
	baseDir     string
	delegate    *sdkauth.FileTokenStore
	verifyStage func(string) error
	replace     func(string, string) error
	removeStage func(string) error
}

func newSecureFileTokenStore(baseDir string) (*secureFileTokenStore, error) {
	absolute, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, errors.New("create secure auth store: resolve directory failed")
	}
	delegate := sdkauth.NewFileTokenStore()
	delegate.SetBaseDir(absolute)
	return &secureFileTokenStore{
		baseDir:     filepath.Clean(absolute),
		delegate:    delegate,
		verifyStage: secureSavedAuthFile,
		replace:     os.Rename,
		removeStage: os.RemoveAll,
	}, nil
}

func (s *secureFileTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	if err := filepath.WalkDir(s.baseDir, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("secure auth store: inspect list entry failed")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("secure auth store: symbolic link entry is not allowed")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("secure auth store: regular list entry required")
		}
		if !authFileHasSingleLink(info) {
			return errors.New("secure auth store: multiply-linked list entry is not allowed")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.delegate.List(ctx)
}

func (s *secureFileTokenStore) Delete(ctx context.Context, id string) error {
	return s.delegate.Delete(ctx, id)
}

func (s *secureFileTokenStore) Save(ctx context.Context, auth *coreauth.Auth) (savedPath string, returnErr error) {
	expected, err := s.authDestination(auth)
	if err != nil {
		return "", err
	}
	root, err := os.Lstat(s.baseDir)
	if err != nil || root.Mode()&os.ModeSymlink != 0 || !root.IsDir() {
		return "", errors.New("secure auth store: prepared directory changed")
	}
	existing, err := os.Lstat(expected)
	if err == nil && (!existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0) {
		return "", errors.New("secure auth store: destination must be a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("secure auth store: inspect destination failed")
	}

	stageDir, err := os.MkdirTemp(s.baseDir, ".llmgw-login-")
	if err != nil {
		return "", errors.New("secure auth store: create stage failed")
	}
	committed := false
	defer func() {
		cleanupErr := cleanupLoginStage(stageDir, s.removeStage)
		if committed {
			return
		}
		if cleanupErr != nil {
			savedPath = ""
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err := os.Chmod(stageDir, 0o700); err != nil {
		return "", errors.New("secure auth store: secure stage failed")
	}

	stageStore := sdkauth.NewFileTokenStore()
	stageStore.SetBaseDir(stageDir)
	stagedAuth := cloneAuthForStage(auth)
	path, err := stageStore.Save(ctx, stagedAuth)
	if err != nil {
		return "", errors.New("secure auth store: stage save failed")
	}
	expectedStage := filepath.Join(stageDir, auth.FileName)
	resolved, err := filepath.Abs(path)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(expectedStage) {
		return "", errors.New("secure auth store: delegate path escaped stage directory")
	}
	if err := s.verifyStage(expectedStage); err != nil {
		return "", errors.New("secure auth store: stage verification failed")
	}
	stageInfo, err := os.Lstat(expectedStage)
	if err != nil || !stageInfo.Mode().IsRegular() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("secure auth store: staged credential must be a regular file")
	}
	if !authFileHasSingleLink(stageInfo) {
		return "", errors.New("secure auth store: staged credential must have one link")
	}
	if err := s.replace(expectedStage, expected); err != nil {
		return "", errors.New("secure auth store: atomic replace failed")
	}
	committed = true
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[coreauth.AttributePath] = expected
	auth.Attributes[coreauth.AttributeSource] = expected
	auth.Attributes[coreauth.AttributeSourceBackend] = coreauth.AuthSourceFile
	return expected, nil
}

func cloneAuthForStage(auth *coreauth.Auth) *coreauth.Auth {
	staged := *auth
	if auth.Attributes != nil {
		staged.Attributes = make(map[string]string, len(auth.Attributes))
		for key, value := range auth.Attributes {
			staged.Attributes[key] = value
		}
	}
	if auth.Metadata != nil {
		staged.Metadata = make(map[string]any, len(auth.Metadata))
		for key, value := range auth.Metadata {
			staged.Metadata[key] = value
		}
	}
	return &staged
}

func cleanupLoginStage(stageDir string, remove func(string) error) error {
	removeErr := remove(stageDir)
	entries, inspectErr := os.ReadDir(stageDir)
	if errors.Is(inspectErr, os.ErrNotExist) {
		return nil
	}
	if inspectErr == nil && len(entries) == 0 {
		return nil
	}
	if removeErr == nil && inspectErr == nil {
		return errors.New("secure auth store: stage cleanup incomplete; manual cleanup required")
	}
	return errors.New("secure auth store: stage cleanup failed; manual cleanup required")
}

func (s *secureFileTokenStore) authDestination(auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", errors.New("secure auth store: auth is required")
	}
	name := auth.FileName
	if strings.TrimSpace(name) == "" || filepath.IsAbs(name) ||
		filepath.VolumeName(name) != "" || filepath.Clean(name) != name ||
		filepath.Base(name) != name || strings.ContainsAny(name, `/\`) ||
		name == "." || name == ".." {
		return "", errors.New("secure auth store: unsafe file name")
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes[coreauth.AttributePath]) != "" {
		return "", errors.New("secure auth store: path attribute is not allowed")
	}
	destination := filepath.Clean(filepath.Join(s.baseDir, name))
	relative, err := filepath.Rel(s.baseDir, destination)
	if err != nil || relative != name || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("secure auth store: file name escapes auth directory")
	}
	return destination, nil
}

func secureSavedAuthFile(path string) error {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("secure saved auth file: regular file required")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return errors.New("secure saved auth file: open failed")
	}
	current, err := file.Stat()
	if err != nil || !os.SameFile(before, current) {
		_ = file.Close()
		return errors.New("secure saved auth file: identity changed")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("secure saved auth file: chmod failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("secure saved auth file: sync failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("secure saved auth file: close failed")
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(current, after) || after.Mode().Perm() != 0o600 {
		return errors.New("secure saved auth file: verification failed")
	}
	return nil
}
