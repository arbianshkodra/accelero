package utils

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

func GetAllServices(services map[string]interface{}) []string {
	var svcNames []string
	for service := range services {
		svcNames = append(svcNames, service)
	}
	return svcNames
}

// SplitServiceNames splits a comma-separated string of service names.
func SplitServiceNames(serviceNames string) []string {
	return strings.Split(serviceNames, ",")
}

func WaitForHealthCheck(ctx context.Context, cli *client.Client, containerID string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Add a short delay to allow Docker to register the health check
	time.Sleep(5 * time.Second)

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("health check timeout for container %s", containerID)
		case <-ticker.C:
			res, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
			if err != nil {
				return fmt.Errorf("failed to inspect container %s: %w", containerID, err)
			}
			state := res.Container.State
			if state == nil {
				return fmt.Errorf("container %s has no state information", containerID)
			}
			// If there is no health check defined, consider the container as healthy
			if state.Health == nil {
				logrus.Infof("No health check defined for container %s, assuming healthy", containerID)
				return nil
			}
			logrus.Debugf("Health status of container %s: %s", containerID, state.Health.Status)
			if state.Health.Status == "healthy" {
				return nil
			} else if state.Health.Status == "unhealthy" {
				return fmt.Errorf("container %s is unhealthy", containerID)
			}
		}
	}
}

func ContainsServiceName(names []string, serviceName string) bool {
	for _, name := range names {
		if strings.Contains(name, serviceName) {
			return true
		}
	}
	return false
}

func LoadEnvFiles(envFiles []string, repoDir string) ([]string, error) {
	var envVars []string

	for _, file := range envFiles {
		filePath := filepath.Join(repoDir, file)
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read env file %s: %w", filePath, err)
		}

		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			envVars = append(envVars, line)
		}
	}

	return envVars, nil
}

// MapPorts converts a list of compose-style port strings into the Moby API
// port types used by container.Config and container.HostConfig.
//
// Accepted forms:
//
//	"80"                         container-only (no publish, acts like expose)
//	"8080:80"                    host_port:container_port
//	"8080:80/udp"                with protocol
//	"127.0.0.1:8080:80"          explicit host_ip
//	"127.0.0.1:8080:80/udp"      everything
//
// Port ranges (e.g. "8000-8010:80-90") and swarm-style mode fields are not
// yet supported.
func MapPorts(ports []string) (network.PortMap, network.PortSet) {
	portMap := network.PortMap{}
	exposedPorts := network.PortSet{}

	defaultHostIP, _ := netip.ParseAddr("0.0.0.0")

	for _, raw := range ports {
		spec, err := parsePortSpec(raw)
		if err != nil {
			logrus.Warnf("Invalid port mapping %q: %v — skipping", raw, err)
			continue
		}

		port, err := network.ParsePort(spec.containerPort + "/" + spec.proto)
		if err != nil {
			logrus.Warnf("Invalid container port in %q: %v — skipping", raw, err)
			continue
		}
		exposedPorts[port] = struct{}{}

		// No host_port means the port is exposed but not published (same as
		// `expose:`) — emit no binding.
		if spec.hostPort == "" {
			continue
		}

		hostIP := defaultHostIP
		if spec.hostIP != "" {
			parsed, err := netip.ParseAddr(spec.hostIP)
			if err != nil {
				logrus.Warnf("Invalid host IP %q in %q: %v — skipping", spec.hostIP, raw, err)
				continue
			}
			hostIP = parsed
		}

		portMap[port] = append(portMap[port], network.PortBinding{
			HostIP:   hostIP,
			HostPort: spec.hostPort,
		})
	}
	return portMap, exposedPorts
}

// portSpec is an internal parsed representation of a compose port string.
type portSpec struct {
	hostIP        string // may be empty
	hostPort      string // may be empty (container-only)
	containerPort string
	proto         string // "tcp" if not specified
}

func parsePortSpec(s string) (portSpec, error) {
	out := portSpec{proto: "tcp"}

	// Split protocol suffix (/tcp | /udp | /sctp), if any.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		// Only treat the tail as a protocol if it looks like one.  This avoids
		// misinterpreting an IPv6 address that happens to contain a '/'.
		rest := s[i+1:]
		if _, err := strconv.Atoi(rest); err != nil && rest != "" && !strings.ContainsAny(rest, ":[]") {
			out.proto = rest
			s = s[:i]
		}
	}

	// Split on ':' from the right; at most 3 components: host_ip:host_port:container_port.
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		out.containerPort = parts[0]
	case 2:
		out.hostPort, out.containerPort = parts[0], parts[1]
	case 3:
		out.hostIP, out.hostPort, out.containerPort = parts[0], parts[1], parts[2]
	default:
		return portSpec{}, fmt.Errorf("too many ':' separators")
	}

	if out.containerPort == "" {
		return portSpec{}, fmt.Errorf("missing container port")
	}
	return out, nil
}
