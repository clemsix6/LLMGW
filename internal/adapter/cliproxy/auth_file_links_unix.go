//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cliproxy

import (
	"os"
	"syscall"
)

// authFileHasSingleLink reports whether a no-follow file observation has one directory entry.
func authFileHasSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
