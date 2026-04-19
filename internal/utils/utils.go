package utils

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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

// MapPorts converts a list of compose-style port mappings ("hostport:containerport")
// into the Moby API port types used by container.Config and container.HostConfig.
func MapPorts(ports []string) (network.PortMap, network.PortSet) {
	portMap := network.PortMap{}
	exposedPorts := network.PortSet{}

	hostIP, _ := netip.ParseAddr("0.0.0.0")

	for _, portStr := range ports {
		hostPort, containerPort, err := net.SplitHostPort(portStr)
		if err != nil {
			logrus.Warnf("Invalid port format %q, skipping", portStr)
			continue
		}
		port, err := network.ParsePort(containerPort + "/tcp")
		if err != nil {
			logrus.Warnf("Invalid container port %q, skipping", containerPort)
			continue
		}
		binding := network.PortBinding{
			HostIP:   hostIP,
			HostPort: hostPort,
		}
		portMap[port] = append(portMap[port], binding)
		exposedPorts[port] = struct{}{}
	}
	return portMap, exposedPorts
}
