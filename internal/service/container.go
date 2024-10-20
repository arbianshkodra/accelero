package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/arbianshkodra/accelero/internal/utils"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
)

// CreateAndStartContainer creates and starts a Docker container.
func CreateAndStartContainer(ctx context.Context, cli *client.Client, name, repoDir string, svc ComposeService, serviceName string) error {
	logrus.Infof("Creating container %s with image %s", name, svc.Image)

	// Load environment variables from env files and inline definitions
	envVars, err := utils.LoadEnvFiles(svc.EnvFile, repoDir)
	if err != nil {
		return fmt.Errorf("failed to load environment files: %w", err)
	}
	envVars = append(envVars, svc.Environment...)

	// Prepare container configuration
	containerConfig := &container.Config{
		Image:  svc.Image,
		Env:    envVars,
		Labels: svc.Labels,
		Cmd:    svc.Command,
	}

	// Add health check if it's defined
	if svc.HealthCheck.Test != nil {
		containerConfig.Healthcheck = &container.HealthConfig{
			Test:        svc.HealthCheck.Test,
			Interval:    ParseDuration(svc.HealthCheck.Interval),
			Timeout:     ParseDuration(svc.HealthCheck.Timeout),
			Retries:     svc.HealthCheck.Retries,
			StartPeriod: ParseDuration(svc.HealthCheck.StartPeriod),
		}
	}

	// Map ports
	portBindings, exposedPorts := utils.MapPorts(svc.Ports)
	containerConfig.ExposedPorts = exposedPorts

	// Prepare host configuration
	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
		Binds:        svc.Volumes,
	}

	// Handle the restart policy
	if svc.Restart != "" {
		restartPolicyMode := container.RestartPolicyMode(svc.Restart)
		hostConfig.RestartPolicy = container.RestartPolicy{
			Name: restartPolicyMode,
		}
	}

	// Parse and set resource limits
	resources := container.Resources{}

	// Parse memory limit
	if svc.MemLimit != "" {
		memBytes, err := units.RAMInBytes(svc.MemLimit)
		if err != nil {
			logrus.Warnf("Invalid memory limit '%s' for service %s: %v", svc.MemLimit, serviceName, err)
		} else {
			resources.Memory = memBytes
		}
	}

	// Parse CPU limit
	if svc.CPULimit != "" {
		cpuUnits, err := parseCPULimit(svc.CPULimit)
		if err != nil {
			logrus.Warnf("Invalid CPU limit '%s' for service %s: %v", svc.CPULimit, serviceName, err)
		} else {
			resources.NanoCPUs = cpuUnits
		}
	}

	// Set the Resources field in hostConfig
	hostConfig.Resources = resources

	// Prepare networking configuration
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{},
	}

	for _, net := range svc.Networks {
		networkingConfig.EndpointsConfig[net] = &network.EndpointSettings{
			Aliases: []string{serviceName},
		}
	}

	logrus.Debugf("Creating container with config: %+v, hostConfig: %+v, networkingConfig: %+v", containerConfig, hostConfig, networkingConfig)

	// Create the container with a timeout context
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := cli.ContainerCreate(ctxWithTimeout, containerConfig, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return fmt.Errorf("failed to create container %s: %w", name, err)
	}

	logrus.Infof("Container created successfully with ID: %s", resp.ID)

	// Start the container
	logrus.Infof("Starting container %s", resp.ID)
	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel()

	if err := cli.ContainerStart(startCtx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container %s: %w", resp.ID, err)
	}

	logrus.Infof("Container started successfully with ID: %s", resp.ID)

	return nil
}

// parseCPULimit parses a CPU limit string (e.g., "0.5") and converts it to NanoCPUs
func parseCPULimit(cpuLimit string) (int64, error) {
	cpuFloat, err := strconv.ParseFloat(cpuLimit, 64)
	if err != nil {
		return 0, err
	}
	return int64(cpuFloat * 1e9), nil // Convert to NanoCPUs
}
