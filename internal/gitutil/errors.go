// Package gitutil holds small git helpers shared between the deployer
// and the reconciler, both of which clone stack repositories.
package gitutil

import (
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// IsPermanentCloneError reports whether a git clone error is one that
// retrying cannot fix: bad or missing credentials, a repository that
// doesn't exist, or a branch/reference that isn't there. Everything
// else (connection resets, timeouts, upstream 5xx) is treated as
// transient and worth a retry.
func IsPermanentCloneError(err error) bool {
	return errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrRepositoryNotFound) ||
		errors.Is(err, plumbing.ErrReferenceNotFound)
}
