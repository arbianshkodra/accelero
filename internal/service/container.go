package service

import (
	"context"
	"fmt"
	"time"

	"github.com/arbianshkodra/accelero/internal/utils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// CreateAndStartContainer creates and starts a Docker container.
func CreateAndStartContainer(ctx context.Context, cli *client.Client, name, repoDir string, svc ComposeService, serviceName string) error {
	logrus.Infof("Creating container %s with image %s", name, svc.Image)

	envVars, err := utils.LoadEnvFiles(svc.EnvFile, repoDir)
	if err != nil {
		return fmt.Errorf("failed to load environment files: %w", err)
	}
	envVars = append(envVars, svc.Environment...)

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

	portBindings, exposedPorts := utils.MapPorts(svc.Ports)

	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
		Binds:        svc.Volumes,
	}

	// Handle the restart policy
	if svc.Restart != "" {
		hostConfig.RestartPolicy = container.RestartPolicy{
			Name: svc.Restart,
		}
	}

	containerConfig.ExposedPorts = exposedPorts

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{},
	}

	for _, net := range svc.Networks {
		networkingConfig.EndpointsConfig[net] = &network.EndpointSettings{
			Aliases: []string{serviceName},
		}
	}

	logrus.Debugf("Creating container with config: %+v, hostConfig: %+v, networkingConfig: %+v", containerConfig, hostConfig, networkingConfig)

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := cli.ContainerCreate(ctxWithTimeout, containerConfig, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return fmt.Errorf("failed to create container %s: %w", name, err)
	}

	logrus.Infof("Container created successfully with ID: %s", resp.ID)

	logrus.Infof("Starting container %s", resp.ID)
	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel()

	if err := cli.ContainerStart(startCtx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start container %s: %w", resp.ID, err)
	}

	logrus.Infof("Container started successfully with ID: %s", resp.ID)

	return nil
}
