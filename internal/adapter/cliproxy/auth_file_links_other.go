//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cliproxy

import "os"

// authFileHasSingleLink fails closed where the platform link count has not been reviewed.
func authFileHasSingleLink(os.FileInfo) bool {
	return false
}
