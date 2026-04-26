package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHashStackSecrets_EmptyReturnsEmptyString(t *testing.T) {
	// Empty/nil set must hash to "" so a freshly-created stack with
	// no secrets matches the zero-value SecretsHash on the record
	// without any special-case logic in the reconciler.
	assert.Equal(t, "", HashStackSecrets(nil))
	assert.Equal(t, "", HashStackSecrets([]*StackSecret{}))
}

func TestHashStackSecrets_StableUnderInputOrder(t *testing.T) {
	// The store returns rows ASC by name, but be defensive: a
	// reconciler getting a differently-ordered slice must compute
	// the same hash. Otherwise routine reconciles would flap drift
	// state every time the source iteration order shifted.
	a := []*StackSecret{
		{Name: "API_KEY", Value: "k1"},
		{Name: "DATABASE_URL", Value: "v1"},
	}
	b := []*StackSecret{
		{Name: "DATABASE_URL", Value: "v1"},
		{Name: "API_KEY", Value: "k1"},
	}
	assert.Equal(t, HashStackSecrets(a), HashStackSecrets(b))
}

func TestHashStackSecrets_DifferentValuesDifferentHash(t *testing.T) {
	// The whole point: rotate a value → hash changes → reconciler
	// reports drift.
	a := []*StackSecret{{Name: "K", Value: "v1"}}
	b := []*StackSecret{{Name: "K", Value: "v2"}}
	assert.NotEqual(t, HashStackSecrets(a), HashStackSecrets(b))
}

func TestHashStackSecrets_NoCollisionFromAdjacentBoundary(t *testing.T) {
	// (name="AB", value="CD") and (name="A", value="BCD") would
	// produce the same byte stream under naïve concatenation. The
	// length-prefix in the hash function defends against that.
	a := []*StackSecret{{Name: "AB", Value: "CD"}}
	b := []*StackSecret{{Name: "A", Value: "BCD"}}
	assert.NotEqual(t, HashStackSecrets(a), HashStackSecrets(b))
}

func TestHashStackSecrets_AddingASecretChangesHash(t *testing.T) {
	// Add a key → hash changes. Remove a key → hash changes back.
	a := []*StackSecret{{Name: "A", Value: "v"}}
	b := []*StackSecret{{Name: "A", Value: "v"}, {Name: "B", Value: "v"}}
	assert.NotEqual(t, HashStackSecrets(a), HashStackSecrets(b))
}
