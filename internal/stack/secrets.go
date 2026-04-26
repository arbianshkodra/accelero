package stack

import (
	"strings"

	"github.com/arbianshkodra/accelero/internal/store"
)

// mergeSecretsIntoEnv merges per-stack secrets into the env slice a
// container will be created with. Secrets win on key collision: the
// operator explicitly `POST /stacks/{id}/secrets`'d the value, so
// overriding a compose-file default is the intended outcome, not a
// conflict. A compose author who wanted the compose value to stay
// authoritative would not have created a secret with that name.
//
// `composeEnv` is a slice of "KEY=VALUE" strings (the same shape Docker
// ContainerCreate's Config.Env takes). Order within composeEnv is
// preserved for keys that are not overridden; overridden keys get
// replaced in place rather than appended, so the final slice's order is
// stable across unrelated secret changes and easy to eyeball.
//
// Secret order is deterministic (the store returns ASC by name), so the
// trailing additions (secrets that didn't shadow anything in
// composeEnv) are also stable.
//
// A nil or empty secrets slice returns composeEnv unchanged — callers
// don't need to guard the no-secrets case.
func mergeSecretsIntoEnv(composeEnv []string, secrets []*store.StackSecret) []string {
	if len(secrets) == 0 {
		return composeEnv
	}

	bySecretName := make(map[string]string, len(secrets))
	for _, s := range secrets {
		bySecretName[s.Name] = s.Value
	}

	out := make([]string, 0, len(composeEnv)+len(secrets))
	overridden := make(map[string]struct{}, len(secrets))

	for _, entry := range composeEnv {
		// Env entries can be "KEY=VALUE" or a bare "KEY" (Docker
		// interprets the latter as "pass through from the daemon's
		// environment"). Both shapes use everything up to the first
		// `=` as the name.
		key := entry
		if eq := strings.IndexByte(entry, '='); eq >= 0 {
			key = entry[:eq]
		}
		if newVal, ok := bySecretName[key]; ok {
			out = append(out, key+"="+newVal)
			overridden[key] = struct{}{}
			continue
		}
		out = append(out, entry)
	}

	// Append secrets that weren't already matched against a compose
	// entry. Still in the store's sorted order — predictable and
	// diff-friendly.
	for _, s := range secrets {
		if _, did := overridden[s.Name]; did {
			continue
		}
		out = append(out, s.Name+"="+s.Value)
	}
	return out
}
