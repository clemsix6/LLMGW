//go:build unix

package cliproxy

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openStartupAuthRoot opens the final source directory without following it.
func openStartupAuthRoot(path string) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, err
	}
	return startupAuthFileFromFD(fd, path)
}

// openStartupAuthFile opens one base name relative to the pinned directory fd.
func openStartupAuthFile(root *os.File, name string) (*os.File, error) {
	if root == nil {
		return nil, errors.New("startup auth root is required")
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	fd, err := unix.Openat(int(root.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return startupAuthFileFromFD(fd, name)
}

// startupAuthFileFromFD transfers one Unix descriptor into an os.File.
func startupAuthFileFromFD(fd int, name string) (*os.File, error) {
	file := os.NewFile(uintptr(fd), name)
	if file != nil {
		return file, nil
	}
	_ = unix.Close(fd)
	return nil, errors.New("wrap startup auth file descriptor")
}
