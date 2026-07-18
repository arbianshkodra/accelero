package gitutil

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/assert"
)

func TestIsPermanentCloneError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"auth required", transport.ErrAuthenticationRequired, true},
		{"auth failed", transport.ErrAuthorizationFailed, true},
		{"repo not found", transport.ErrRepositoryNotFound, true},
		{"reference not found", plumbing.ErrReferenceNotFound, true},
		{"wrapped auth required", fmt.Errorf("git clone: %w", transport.ErrAuthenticationRequired), true},
		{"transient network error", errors.New("dial tcp: connection reset"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsPermanentCloneError(tc.err))
		})
	}
}
