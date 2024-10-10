package service

import (
	"context"
	"fmt"
	"time"

	"github.com/arbianshkodra/accelero/internal/utils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type ComposeService struct {
	Image       string            `yaml:"image"`
	Environment EnvVars           `yaml:"environment,omitempty"`
	EnvFile     []string          `yaml:"env_file,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Command     []string          `yaml:"command,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	HealthCheck HealthCheck       `yaml:"healthcheck,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
	Restart     string            `yaml:"restart,omitempty"`
}

func AreContainersRunning(cli *client.Client, serviceName string) (bool, error) {
	ctx := context.Background()
	filter := filters.NewArgs()
	filter.Add("name", serviceName)

	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Filters: filter})
	if err != nil {
		return false, fmt.Errorf("failed to list containers: %w", err)
	}

	return len(containers) > 0, nil
}

func DeployService(cli *client.Client, serviceName, repoDir string, svc ComposeService, scale int) error {
	ctx := context.Background()

	// List existing containers
	existingContainers, err := cli.ContainerList(ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}
	logrus.Debugf("Found %d existing containers", len(existingContainers))

	latestImageTag := svc.Image
	var containersToRemove []string
	containersFound := false

	// Inspect existing containers and determine which to remove
	for _, container := range existingContainers {
		if !utils.ContainsServiceName(container.Names, serviceName) {
			continue
		}
		containersFound = true

		logrus.Infof("Found existing container %s with image tag: %s", container.ID, container.Image)
		logrus.Infof("New image tag: %s", latestImageTag)

		containerInfo, err := cli.ContainerInspect(ctx, container.ID)
		if err != nil {
			return fmt.Errorf("failed to inspect container %s: %w", container.ID, err)
		}

		if container.Image != latestImageTag {
			logrus.Infof("Preparing to remove existing container %s with outdated image tag", container.ID)
			containersToRemove = append(containersToRemove, container.ID)
		} else if containerInfo.State.Health != nil && containerInfo.State.Health.Status != "healthy" {
			logrus.Infof("Waiting for health check to complete for container %s", container.ID)
			if err := utils.WaitForHealthCheck(ctx, cli, container.ID); err != nil {
				return fmt.Errorf("health check failed for container %s: %w", container.ID, err)
			}
		} else {
			logrus.Infof("Existing container %s has the same image tag, no action needed", container.ID)
		}
	}

	// If no containers were found, it's the first deployment
	if !containersFound {
		logrus.Infof("No existing containers found, deploying service %s for the first time", serviceName)
		for i := 0; i < scale; i++ {
			instanceName := fmt.Sprintf("%s_%d_%d", serviceName, i, time.Now().UnixNano())
			// Update the function call here
			if err := CreateAndStartContainer(ctx, cli, instanceName, repoDir, svc, serviceName); err != nil {
				return fmt.Errorf("failed to create and start container %s: %w", instanceName, err)
			}
			logrus.Infof("Waiting for health check to complete for new container %s", instanceName)
			if err := utils.WaitForHealthCheck(ctx, cli, instanceName); err != nil {
				return fmt.Errorf("health check failed for new container %s: %w", instanceName, err)
			}
			logrus.Infof("New container %s created and started successfully", instanceName)
		}
	} else if len(containersToRemove) > 0 {
		logrus.Infof("Creating and starting new containers for service: %s", serviceName)
		for i := 0; i < scale; i++ {
			instanceName := fmt.Sprintf("%s_%d_%d", serviceName, i, time.Now().UnixNano())
			// Update the function call here
			if err := CreateAndStartContainer(ctx, cli, instanceName, repoDir, svc, serviceName); err != nil {
				return fmt.Errorf("failed to create and start container %s: %w", instanceName, err)
			}
			logrus.Infof("Waiting for health check to complete for new container %s", instanceName)
			if err := utils.WaitForHealthCheck(ctx, cli, instanceName); err != nil {
				return fmt.Errorf("health check failed for new container %s: %w", instanceName, err)
			}
			logrus.Infof("New container %s created and started successfully", instanceName)
		}

		for _, containerID := range containersToRemove {
			logrus.Infof("Removing container %s", containerID)
			if err := cli.ContainerRemove(ctx, containerID, types.ContainerRemoveOptions{Force: true}); err != nil {
				return fmt.Errorf("failed to remove container %s: %w", containerID, err)
			}
		}
	} else {
		logrus.Info("No existing containers with outdated image tags found, no action needed")
	}

	return nil
}
