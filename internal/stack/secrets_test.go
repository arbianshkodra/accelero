package stack

import (
	"testing"

	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secret is a test helper that builds a store.StackSecret literal with
// only the fields this file cares about. CreatedAt/UpdatedAt don't
// matter for merge tests; the real store fills them in.
func secret(name, value string) *store.StackSecret {
	return &store.StackSecret{Name: name, Value: value}
}

func TestMergeSecretsIntoEnv_NoSecretsReturnsInputUnchanged(t *testing.T) {
	// Fast path. The helper runs on every container create, so "no
	// secrets" has to stay allocation-free — we return the caller's
	// slice as-is rather than copying. Equality check below is a
	// reference check via identical length + content.
	in := []string{"A=1", "B=2"}
	out := mergeSecretsIntoEnv(in, nil)
	assert.Equal(t, in, out)

	out = mergeSecretsIntoEnv(in, []*store.StackSecret{})
	assert.Equal(t, in, out)
}

func TestMergeSecretsIntoEnv_AppendsNewKeysPreservingComposeOrder(t *testing.T) {
	compose := []string{"LOG_LEVEL=info", "PORT=8080"}
	secrets := []*store.StackSecret{
		secret("API_KEY", "k1"),
		secret("DATABASE_URL", "postgres://"),
	}

	got := mergeSecretsIntoEnv(compose, secrets)
	// Compose entries come out first, in original order; secrets
	// follow in store order (already ASC by name in the real store).
	require.Equal(t, []string{
		"LOG_LEVEL=info",
		"PORT=8080",
		"API_KEY=k1",
		"DATABASE_URL=postgres://",
	}, got)
}

func TestMergeSecretsIntoEnv_SecretOverridesComposeValueInPlace(t *testing.T) {
	// The key security+correctness property: if an operator POSTs a
	// DATABASE_URL secret, the container sees the secret's value even
	// if the compose file defines DATABASE_URL=some-default. Operator
	// intent beats compose default — this is the whole reason the
	// secrets API exists.
	compose := []string{"LOG_LEVEL=info", "DATABASE_URL=compose-default", "PORT=8080"}
	secrets := []*store.StackSecret{
		secret("DATABASE_URL", "postgres://override"),
	}

	got := mergeSecretsIntoEnv(compose, secrets)
	// Override happens *in place* — position 1 stays DATABASE_URL —
	// so the final slice's order is stable across unrelated changes
	// and the diff against "compose env only" is minimal.
	require.Equal(t, []string{
		"LOG_LEVEL=info",
		"DATABASE_URL=postgres://override",
		"PORT=8080",
	}, got)
}

func TestMergeSecretsIntoEnv_HandlesBareKeyComposeEntries(t *testing.T) {
	// Docker allows a bare "KEY" entry (no `=`) meaning "pass this
	// name through from the daemon environment". If a secret with
	// the same name exists, we replace the pass-through with an
	// explicit value. Otherwise we leave the bare form untouched.
	compose := []string{"HOME", "DATABASE_URL", "PATH"}
	secrets := []*store.StackSecret{
		secret("DATABASE_URL", "postgres://"),
	}

	got := mergeSecretsIntoEnv(compose, secrets)
	require.Equal(t, []string{
		"HOME",
		"DATABASE_URL=postgres://",
		"PATH",
	}, got)
}

func TestMergeSecretsIntoEnv_DoesNotMutateComposeSlice(t *testing.T) {
	// The deployer reuses the compose env across rollback paths; the
	// merge must not touch the caller's slice. A surprise mutation
	// would surface as a secret value appearing in the *rolled-back*
	// container after a failed deploy.
	compose := []string{"A=1", "DATABASE_URL=default"}
	composeCopy := append([]string(nil), compose...)

	_ = mergeSecretsIntoEnv(compose, []*store.StackSecret{
		secret("DATABASE_URL", "secret-value"),
	})

	assert.Equal(t, composeCopy, compose, "source slice must remain unchanged")
}

func TestMergeSecretsIntoEnv_EmptyValueSecretStillOverrides(t *testing.T) {
	// The secrets API rejects empty values on write — so this case
	// doesn't come from user-provided data. It's here to nail down
	// behaviour if it ever slips in (e.g. through a direct store
	// UpsertStackSecret call in a future code path): we still honour
	// the override, producing `KEY=` rather than keeping the compose
	// default.
	compose := []string{"KEY=default"}
	got := mergeSecretsIntoEnv(compose, []*store.StackSecret{secret("KEY", "")})
	require.Equal(t, []string{"KEY="}, got)
}
