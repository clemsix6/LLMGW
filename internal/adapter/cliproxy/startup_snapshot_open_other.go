//go:build !unix

package cliproxy

import (
	"errors"
	"os"
)

var errStartupAuthNoFollowUnsupported = errors.New(
	"no-follow startup auth snapshot is unsupported on this platform",
)

// openStartupAuthRoot fails closed where no reviewed no-follow API exists.
func openStartupAuthRoot(string) (*os.File, error) {
	return nil, errStartupAuthNoFollowUnsupported
}

// openStartupAuthFile fails closed where no reviewed openat API exists.
func openStartupAuthFile(*os.File, string) (*os.File, error) {
	return nil, errStartupAuthNoFollowUnsupported
}
