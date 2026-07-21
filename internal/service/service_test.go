package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"10s", 10 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", 1 * time.Hour},
		{"", 0},
		{"invalid", 0},
	}

	for _, tc := range tests {
		result := ParseDuration(tc.input)
		assert.Equal(t, tc.expected, result, "ParseDuration(%q)", tc.input)
	}
}

func TestEnvVarsUnmarshalYAML_List(t *testing.T) {
	var envs EnvVars
	err := envs.UnmarshalYAML(func(v interface{}) error {
		raw, ok := v.(*[]string)
		if ok {
			*raw = []string{"KEY=value", "FOO=bar"}
			return nil
		}
		return assert.AnError
	})
	assert.NoError(t, err)
	assert.Equal(t, EnvVars{"KEY=value", "FOO=bar"}, envs)
}
