package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/arbianshkodra/accelero/internal/utils"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type ComposeService struct {
	Image       string            `yaml:"image"`
	Environment utils.EnvVars     `yaml:"environment,omitempty"`
	EnvFile     []string          `yaml:"env_file,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Command     []string          `yaml:"command,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	HealthCheck utils.HealthCheck `yaml:"healthcheck,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
	Restart     string            `yaml:"restart,omitempty"`
}

func PullImage(cli *client.Client, image string) error {
	ctx := context.Background()

	// Check if Docker registry credentials are provided
	username := os.Getenv("DOCKER_USERNAME")
	password := os.Getenv("DOCKER_PASSWORD")
	serverAddress := os.Getenv("DOCKER_REGISTRY")

	var authConfig registry.AuthConfig
	var authStr string
	if username != "" && password != "" {
		authConfig = registry.AuthConfig{
			Username:      username,
			Password:      password,
			ServerAddress: serverAddress,
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			return fmt.Errorf("failed to encode auth config: %w", err)
		}
		authStr = base64.URLEncoding.EncodeToString(encodedJSON)
	}

	options := types.ImagePullOptions{}
	if authStr != "" {
		options.RegistryAuth = authStr
	}

	out, err := cli.ImagePull(ctx, image, options)
	if err != nil {
		return fmt.Errorf("error pulling image %s: %w", image, err)
	}
	defer out.Close()

	// Read the output to ensure the image is pulled
	buf := make([]byte, 1024)
	for {
		_, err := out.Read(buf)
		if err != nil {
			break
		}
	}

	logrus.Infof("Successfully pulled image: %s", image)
	return nil
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
			if err := createAndStartContainer(ctx, cli, instanceName, repoDir, svc, serviceName); err != nil {
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
			if err := createAndStartContainer(ctx, cli, instanceName, repoDir, svc, serviceName); err != nil {
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

func createAndStartContainer(ctx context.Context, cli *client.Client, name, repoDir string, svc ComposeService, serviceName string) error {
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
			Interval:    utils.ParseDuration(svc.HealthCheck.Interval),
			Timeout:     utils.ParseDuration(svc.HealthCheck.Timeout),
			Retries:     svc.HealthCheck.Retries,
			StartPeriod: utils.ParseDuration(svc.HealthCheck.StartPeriod),
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

	// logrus.Debugf("Creating container with config: %+v, hostConfig: %+v, networkingConfig: %+v", containerConfig, hostConfig, networkingConfig)

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
