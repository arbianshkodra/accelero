package service

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// ServiceState captures the state of a service at a point in time
type ServiceState struct {
	ServiceName     string
	ContainerStates []ContainerState
	ImageTag        string
	CreatedAt       time.Time
}

// ContainerState captures the state of a container
type ContainerState struct {
	ID         string
	Name       string
	Image      string
	Config     *container.Config
	HostConfig *container.HostConfig
	NetworkIDs []string
}

// CaptureServiceState captures the current state of a service including its containers
func CaptureServiceState(ctx context.Context, cli *client.Client, serviceName string) (*ServiceState, error) {
	logrus.Infof("Capturing current state of service %s for possible rollback", serviceName)

	// List containers for this service
	containers, err := getServiceContainers(ctx, cli, serviceName)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers for service %s: %w", serviceName, err)
	}

	if len(containers) == 0 {
		logrus.Warnf("No containers found for service %s - rollback may not be possible", serviceName)
		return &ServiceState{
			ServiceName:     serviceName,
			ContainerStates: []ContainerState{},
			CreatedAt:       time.Now(),
		}, nil
	}

	// For each container, capture its state
	var containerStates []ContainerState
	var imageTag string

	for _, c := range containers {
		logrus.Debugf("Capturing state for container %s of service %s", c.ID[:12], serviceName)
		containerInfo, err := cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container %s: %w", c.ID, err)
		}

		// Get network IDs
		var networkIDs []string
		for networkName := range containerInfo.NetworkSettings.Networks {
			networkIDs = append(networkIDs, networkName)
		}

		containerState := ContainerState{
			ID:         c.ID,
			Name:       c.Names[0],
			Image:      c.Image,
			Config:     containerInfo.Config,
			HostConfig: containerInfo.HostConfig,
			NetworkIDs: networkIDs,
		}
		containerStates = append(containerStates, containerState)

		// We use the image from the first container as the service image
		if imageTag == "" {
			imageTag = c.Image
		}
	}

	return &ServiceState{
		ServiceName:     serviceName,
		ContainerStates: containerStates,
		ImageTag:        imageTag,
		CreatedAt:       time.Now(),
	}, nil
}

// RollbackService restores a service to a previous state
func RollbackService(ctx context.Context, cli *client.Client, state *ServiceState) error {
	if state == nil {
		return fmt.Errorf("cannot rollback with nil service state")
	}

	logrus.Warnf("Rolling back service %s to previous state with image %s", state.ServiceName, state.ImageTag)

	if len(state.ContainerStates) == 0 {
		logrus.Warnf("No container states found for service %s - rollback not possible", state.ServiceName)
		return fmt.Errorf("no containers found in previous state")
	}

	// Stop and remove current containers for this service
	currentContainers, err := getServiceContainers(ctx, cli, state.ServiceName)
	if err != nil {
		return fmt.Errorf("failed to list current containers for service %s: %w", state.ServiceName, err)
	}

	for _, c := range currentContainers {
		logrus.Infof("Stopping and removing container %s during rollback", c.ID[:12])
		// Stop the container with a timeout
		stopTimeout := int(10)
		if err := cli.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &stopTimeout}); err != nil {
			logrus.Warnf("Failed to stop container %s, forcing removal: %v", c.ID[:12], err)
		}

		if err := cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("failed to remove container %s: %w", c.ID, err)
		}
	}

	// Recreate containers from previous state
	for i, containerState := range state.ContainerStates {
		newName := fmt.Sprintf("%s_rollback_%d_%d", state.ServiceName, i, time.Now().UnixNano())
		logrus.Infof("Recreating container with name %s from image %s", newName, containerState.Image)

		// Create container from saved config
		resp, err := cli.ContainerCreate(
			ctx,
			containerState.Config,
			containerState.HostConfig,
			nil, // No network config, we'll connect later
			nil, // No platform
			newName,
		)
		if err != nil {
			return fmt.Errorf("failed to create container during rollback: %w", err)
		}

		// Connect to networks
		for _, networkID := range containerState.NetworkIDs {
			if err := cli.NetworkConnect(ctx, networkID, resp.ID, nil); err != nil {
				logrus.Warnf("Failed to connect container %s to network %s: %v", resp.ID[:12], networkID, err)
			}
		}

		// Start the container
		if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			return fmt.Errorf("failed to start container %s: %w", resp.ID, err)
		}

		logrus.Infof("Successfully recreated container %s during rollback", resp.ID[:12])
	}

	logrus.Infof("Rollback of service %s completed", state.ServiceName)
	return nil
}

// getServiceContainers lists all containers for a specific service
func getServiceContainers(ctx context.Context, cli *client.Client, serviceName string) ([]types.Container, error) {
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var serviceContainers []types.Container
	for _, c := range containers {
		for _, name := range c.Names {
			if containsServiceName(name, serviceName) {
				serviceContainers = append(serviceContainers, c)
				break
			}
		}
	}
	return serviceContainers, nil
}

// containsServiceName checks if a container name contains the service name
func containsServiceName(containerName, serviceName string) bool {
	// Strip leading slash if present (Docker names often have it)
	if len(containerName) > 0 && containerName[0] == '/' {
		containerName = containerName[1:]
	}

	// Check if the container name starts with the service name
	// We check for service name followed by underscore to avoid partial matches
	// e.g. "web_1" matches service "web" but not "webproxy"
	return len(containerName) >= len(serviceName) &&
		containerName[:len(serviceName)] == serviceName &&
		(len(containerName) == len(serviceName) || containerName[len(serviceName)] == '_')
}
