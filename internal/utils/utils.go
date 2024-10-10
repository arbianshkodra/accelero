package utils

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
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
			containerInfo, err := cli.ContainerInspect(ctx, containerID)
			if err != nil {
				return fmt.Errorf("failed to inspect container %s: %w", containerID, err)
			}
			if containerInfo.State == nil {
				return fmt.Errorf("container %s has no state information", containerID)
			}
			// If there is no health check defined, consider the container as healthy
			if containerInfo.State.Health == nil {
				logrus.Infof("No health check defined for container %s, assuming healthy", containerID)
				return nil
			}
			logrus.Debugf("Health status of container %s: %s", containerID, containerInfo.State.Health.Status)
			if containerInfo.State.Health.Status == "healthy" {
				return nil
			} else if containerInfo.State.Health.Status == "unhealthy" {
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

func MapPorts(ports []string) (nat.PortMap, nat.PortSet) {
	portMap := nat.PortMap{}
	exposedPorts := nat.PortSet{}
	for _, port := range ports {
		hostPort, containerPort, err := net.SplitHostPort(port)
		if err != nil {
			logrus.Warnf("Invalid port format '%s', skipping", port)
			continue
		}
		portBinding := nat.PortBinding{
			HostIP:   "0.0.0.0",
			HostPort: hostPort,
		}
		containerNatPort := nat.Port(containerPort + "/tcp")
		portMap[containerNatPort] = append(portMap[containerNatPort], portBinding)
		exposedPorts[containerNatPort] = struct{}{}
	}
	return portMap, exposedPorts
}
