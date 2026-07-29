package cliproxy

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/clemsix6/LLMGW/internal/config"
)

const (
	maximumStartupAuthFiles     = 256
	maximumStartupAuthFileBytes = 1 << 20
	maximumStartupAuthBytes     = 16 << 20
)

// sdkStartupSnapshot owns the only SDK-visible configuration and auth tree.
type sdkStartupSnapshot struct {
	config  config.Config
	authDir string

	cleanupOnce   sync.Once
	cleanupErr    error
	removeAuthDir func(string) error
}

// newSDKStartupSnapshot freezes every mutable SDK startup input.
func newSDKStartupSnapshot(cfg config.Config) (*sdkStartupSnapshot, error) {
	if cfg.Proxy == nil {
		return nil, errors.New("snapshot SDK startup configuration: configuration is required")
	}
	frozen := cfg
	frozen.Proxy = cfg.Proxy.CloneForRuntime()
	if frozen.Proxy == nil {
		return nil, errors.New("snapshot SDK startup configuration: clone failed")
	}

	authDir, err := copyStartupAuthDir(frozen.Proxy.AuthDir)
	if err != nil {
		return nil, err
	}
	snapshot := &sdkStartupSnapshot{
		config:        frozen,
		authDir:       authDir,
		removeAuthDir: os.RemoveAll,
	}
	frozen.Proxy.AuthDir = authDir
	snapshot.config = frozen
	if err := snapshot.config.ValidateUsageBackpressure(); err != nil {
		cleanupErr := snapshot.Cleanup()
		return nil, errors.Join(err, cleanupErr)
	}
	return snapshot, nil
}

// Cleanup removes the private auth copy exactly once.
func (s *sdkStartupSnapshot) Cleanup() error {
	if s == nil {
		return nil
	}
	s.cleanupOnce.Do(func() {
		if s.authDir == "" {
			return
		}
		remove := s.removeAuthDir
		if remove == nil {
			remove = os.RemoveAll
		}
		if err := remove(s.authDir); err != nil {
			s.cleanupErr = errors.New("remove SDK startup auth snapshot: unavailable")
		}
	})
	return s.cleanupErr
}

// startupAuthLimits tracks cumulative content copied into one snapshot.
type startupAuthLimits struct {
	bytes int64 // bytes is the cumulative copied content size.
}

// startupAuthEntry pins one inspected source name and its initial identity.
type startupAuthEntry struct {
	name string      // name is one root-relative JSON base name.
	info os.FileInfo // info is the no-follow pre-open identity.
}

// copyStartupAuthDir pins the source tree and copies only regular JSON files.
func copyStartupAuthDir(source string) (snapshot string, err error) {
	snapshot, err = newPrivateAuthDir()
	if err != nil {
		return "", err
	}
	cleanupOnError := func(cause error) (string, error) {
		_ = os.RemoveAll(snapshot)
		return "", cause
	}
	source, err = resolveStartupAuthDir(source)
	if err != nil {
		return cleanupOnError(err)
	}
	root, err := openStartupAuthRoot(source)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return cleanupOnError(
			errors.New("open SDK startup auth directory without symbolic links: unavailable"),
		)
	}
	defer root.Close()
	if err := copyFlatAuthFiles(root, snapshot); err != nil {
		return cleanupOnError(err)
	}
	return snapshot, nil
}

// newPrivateAuthDir creates the destination with owner-only permissions.
func newPrivateAuthDir() (string, error) {
	snapshot, err := os.MkdirTemp("", "llmgw-auth-")
	if err != nil {
		return "", errors.New("create SDK startup auth snapshot: unavailable")
	}
	if err := os.Chmod(snapshot, 0o700); err != nil {
		_ = os.RemoveAll(snapshot)
		return "", errors.New("secure SDK startup auth snapshot: unavailable")
	}
	return snapshot, nil
}

// copyFlatAuthFiles validates every entry before copying stable file contents.
func copyFlatAuthFiles(
	root *os.File,
	destination string,
) error {
	names, err := readBoundedAuthNames(root)
	if err != nil {
		return err
	}
	entries, err := inspectFlatAuthEntries(root, names)
	if err != nil {
		return err
	}
	limits := startupAuthLimits{}
	for _, entry := range entries {
		data, err := openAndReadAuthFile(root, entry)
		if err != nil {
			return err
		}
		limits.bytes += int64(len(data))
		if limits.bytes > maximumStartupAuthBytes {
			return errors.New("snapshot SDK startup auth files: total size limit exceeded")
		}
		if err := writePrivateAuthFile(destination, entry.name, data); err != nil {
			return err
		}
	}
	return nil
}

// readBoundedAuthNames counts every root entry without allocating past the cap.
func readBoundedAuthNames(root *os.File) ([]string, error) {
	names := make([]string, 0, maximumStartupAuthFiles)
	for {
		batch, err := root.Readdirnames(32)
		names = append(names, batch...)
		if len(names) > maximumStartupAuthFiles {
			return nil, errors.New("snapshot SDK startup auth entries: count limit exceeded")
		}
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, errors.New("read SDK startup auth directory: unavailable")
		}
		if len(batch) == 0 {
			return nil, errors.New("read SDK startup auth directory: unavailable")
		}
	}
}

// inspectFlatAuthEntries rejects non-JSON and non-regular root entries.
func inspectFlatAuthEntries(root *os.File, names []string) ([]startupAuthEntry, error) {
	entries := make([]startupAuthEntry, 0, len(names))
	for _, name := range names {
		if name == "" || filepath.Base(name) != name ||
			!strings.EqualFold(filepath.Ext(name), ".json") {
			return nil, errors.New("snapshot SDK startup auth entry: JSON file required")
		}
		info, err := os.Lstat(filepath.Join(root.Name(), name))
		if err != nil {
			return nil, errors.New("inspect SDK startup auth entry: unavailable")
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("snapshot SDK startup auth entry: regular file required")
		}
		if info.Size() < 0 || info.Size() > maximumStartupAuthFileBytes {
			return nil, errors.New("snapshot SDK startup auth file: size limit exceeded")
		}
		entries = append(entries, startupAuthEntry{name: name, info: info})
	}
	return entries, nil
}

// openAndReadAuthFile opens one inspected name relative to the pinned root.
func openAndReadAuthFile(
	root *os.File,
	entry startupAuthEntry,
) ([]byte, error) {
	name := entry.name
	file, err := openStartupAuthFile(root, name)
	if err != nil {
		return nil, errors.New("open SDK startup auth file without symbolic links: unavailable")
	}
	defer file.Close()
	return readStableAuthFile(file, entry.info, name)
}

// readStableAuthFile rejects identity, metadata, or content changes during copy.
func readStableAuthFile(
	file *os.File,
	expected os.FileInfo,
	name string,
) ([]byte, error) {
	before, err := file.Stat()
	if err != nil || !sameAuthFileState(expected, before) {
		return nil, errors.New("pin SDK startup auth file: changed during snapshot")
	}
	first, err := readBoundedAuthFile(file)
	if err != nil {
		return nil, err
	}
	middle, err := file.Stat()
	if err != nil || !sameAuthFileState(before, middle) {
		return nil, errors.New("verify SDK startup auth file: changed during snapshot")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("rewind SDK startup auth file: unavailable")
	}
	second, err := readBoundedAuthFile(file)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !sameAuthFileState(middle, after) ||
		sha256.Sum256(first) != sha256.Sum256(second) ||
		!bytes.Equal(first, second) {
		return nil, errors.New("verify SDK startup auth file: changed during snapshot")
	}
	return first, nil
}

// readBoundedAuthFile reads at most one byte beyond the per-file cap.
func readBoundedAuthFile(file *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maximumStartupAuthFileBytes+1))
	if err != nil {
		return nil, errors.New("read SDK startup auth file: unavailable")
	}
	if len(data) > maximumStartupAuthFileBytes {
		return nil, errors.New("snapshot SDK startup auth file: size limit exceeded")
	}
	return data, nil
}

// sameAuthFileState compares stable identity and portable metadata.
func sameAuthFileState(expected os.FileInfo, actual os.FileInfo) bool {
	return expected != nil &&
		actual != nil &&
		actual.Mode().IsRegular() &&
		os.SameFile(expected, actual) &&
		expected.Size() == actual.Size() &&
		expected.ModTime().Equal(actual.ModTime()) &&
		expected.Mode() == actual.Mode()
}

// writePrivateAuthFile creates one final destination exactly once.
func writePrivateAuthFile(destination string, name string, data []byte) error {
	path := filepath.Join(destination, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create SDK startup auth file: unavailable")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("secure SDK startup auth file: unavailable")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return errors.New("write SDK startup auth file: unavailable")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync SDK startup auth file: unavailable")
	}
	if err := file.Close(); err != nil {
		return errors.New("close SDK startup auth file: unavailable")
	}
	return nil
}

// resolveStartupAuthDir mirrors the SDK's startup tilde expansion.
func resolveStartupAuthDir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("resolve SDK startup auth directory: required")
	}
	if !strings.HasPrefix(value, "~") {
		return filepath.Clean(value), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve SDK startup auth directory: unavailable")
	}
	remainder := strings.TrimLeft(strings.TrimPrefix(value, "~"), "/\\")
	if remainder == "" {
		return filepath.Clean(home), nil
	}
	remainder = strings.ReplaceAll(remainder, "\\", "/")
	return filepath.Clean(filepath.Join(home, filepath.FromSlash(remainder))), nil
}
