package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitServiceNames(t *testing.T) {
	input := "service1,service2,service3"
	expected := []string{"service1", "service2", "service3"}

	result := SplitServiceNames(input)
	assert.Equal(t, expected, result)
}

func TestContainsServiceName(t *testing.T) {
	names := []string{"/service1_instance1", "/service2_instance1"}
	serviceName := "service1"

	result := ContainsServiceName(names, serviceName)
	assert.True(t, result)

	serviceName = "service3"
	result = ContainsServiceName(names, serviceName)
	assert.False(t, result)
}
