package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// HashStackSecrets returns a stable hex digest of the (name, value)
// pairs in `secrets`. The reconciler compares this against the hash
// persisted on the Stack record to detect "secrets rotated since the
// last successful deploy" drift.
//
// An empty/nil set hashes to "" rather than to sha256("") so that a
// freshly-created stack with no secrets matches the zero-value
// SecretsHash on the record without any special-case logic in the
// reconciler.
//
// The fields are sorted by name and length-prefixed before being fed
// into SHA-256, so (name="AB", value="CD") and (name="A", value="BCD")
// don't collide: "2:AB=2:CD\n" vs "1:A=3:BCD\n".
//
// Lives in the store package — not internal/stack — so the reconciler
// can use it without pulling the stack package (and its Docker /
// compose dependencies) into the import graph.
func HashStackSecrets(secrets []*StackSecret) string {
	if len(secrets) == 0 {
		return ""
	}
	byName := make(map[string]string, len(secrets))
	names := make([]string, 0, len(secrets))
	for _, s := range secrets {
		byName[s.Name] = s.Value
		names = append(names, s.Name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(name), name, len(byName[name]), byName[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}
