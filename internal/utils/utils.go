package utils

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type EnvVars []string

func (e *EnvVars) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw []string
	if err := unmarshal(&raw); err == nil {
		*e = raw
		return nil
	}

	var rawMap map[string]string
	if err := unmarshal(&rawMap); err == nil {
		for k, v := range rawMap {
			*e = append(*e, fmt.Sprintf("%s=%s", k, v))
		}
		return nil
	}

	return fmt.Errorf("failed to unmarshal environment variables")
}

type HealthCheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
}

func GetAllServices(services map[string]interface{}) []string {
	var svcNames []string
	for service := range services {
		svcNames = append(svcNames, service)
	}
	return svcNames
}

// Utility function to split service names
func SplitServiceNames(serviceNames string) []string {
	return strings.Split(serviceNames, ",")
}

func WaitForHealthCheck(ctx context.Context, cli *client.Client, containerID string) error {
	timeout := time.After(90 * time.Second)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop() // Ensure the ticker is stopped when the function exits

	// Add a short delay to allow Docker to register the health check
	time.Sleep(5 * time.Second)

	for {
		select {
		case <-timeout:
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
				log.Printf("No health check defined for container %s, assuming healthy", containerID)
				return nil
			}
			log.Printf("Health status of container %s: %s", containerID, containerInfo.State.Health.Status)
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
		filePath := file // filepath.Join(repoDir, file)
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
		hostPort, containerPort, _ := net.SplitHostPort(port)
		portBinding := nat.PortBinding{
			HostIP:   "0.0.0.0",
			HostPort: hostPort,
		}
		port := nat.Port(containerPort + "/tcp")
		portMap[port] = append(portMap[port], portBinding)
		exposedPorts[port] = struct{}{}
	}
	return portMap, exposedPorts
}

func ParseDuration(duration string) time.Duration {
	if duration == "" {
		return 0
	}
	parsedDuration, err := time.ParseDuration(duration)
	if err != nil {
		log.Printf("Failed to parse duration: %s, using default 0", duration)
		return 0
	}
	return parsedDuration
}
