package utils

import (
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		in       string
		wantHIP  string
		wantHP   string
		wantCP   string
		wantProto string
	}{
		{"80", "", "", "80", "tcp"},
		{"8080:80", "", "8080", "80", "tcp"},
		{"8080:80/udp", "", "8080", "80", "udp"},
		{"127.0.0.1:8080:80", "127.0.0.1", "8080", "80", "tcp"},
		{"127.0.0.1:8080:80/udp", "127.0.0.1", "8080", "80", "udp"},
		{"::1:8080:80", ":", "", "", ""}, // IPv6-ish — expect an error below
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePortSpec(tc.in)
			if tc.wantCP == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantHIP, got.hostIP)
			assert.Equal(t, tc.wantHP, got.hostPort)
			assert.Equal(t, tc.wantCP, got.containerPort)
			assert.Equal(t, tc.wantProto, got.proto)
		})
	}
}

func TestMapPorts(t *testing.T) {
	// Three unique ports across three accepted forms.
	portMap, exposed := MapPorts([]string{
		"8080:80",            // default host IP, tcp
		"8081:81/udp",        // protocol override
		"127.0.0.1:8082:82",  // explicit host IP
	})

	// Each unique container-port/proto pair ends up exposed.
	assert.Len(t, exposed, 3)

	p80, _ := network.ParsePort("80/tcp")
	require.Len(t, portMap[p80], 1)
	assert.Equal(t, "8080", portMap[p80][0].HostPort)
	assert.Equal(t, "0.0.0.0", portMap[p80][0].HostIP.String())

	p81, _ := network.ParsePort("81/udp")
	require.Len(t, portMap[p81], 1)
	assert.Equal(t, "8081", portMap[p81][0].HostPort)
	assert.Equal(t, "0.0.0.0", portMap[p81][0].HostIP.String())

	p82, _ := network.ParsePort("82/tcp")
	require.Len(t, portMap[p82], 1)
	assert.Equal(t, "8082", portMap[p82][0].HostPort)
	assert.Equal(t, "127.0.0.1", portMap[p82][0].HostIP.String())
}

func TestMapPorts_ContainerOnlyDoesNotPublish(t *testing.T) {
	// A bare container-port entry exposes but does not publish.
	portMap, exposed := MapPorts([]string{"3000"})

	assert.Len(t, exposed, 1)
	p, _ := network.ParsePort("3000/tcp")
	assert.Empty(t, portMap[p], "bare container-port must not publish a binding")
}

func TestMapPorts_InvalidEntriesAreSkipped(t *testing.T) {
	// Invalid entries should log + skip, not crash or fail the deploy.
	portMap, exposed := MapPorts([]string{
		"not-a-port",
		"8080:80", // valid, should survive
	})
	assert.Len(t, exposed, 1)

	p, _ := network.ParsePort("80/tcp")
	require.Len(t, portMap[p], 1)
	assert.Equal(t, "8080", portMap[p][0].HostPort)
}
