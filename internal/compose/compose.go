package compose

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"gopkg.in/yaml.v2"
)

type ComposeFile struct {
	Version  string                    `yaml:"version"`
	Services map[string]ComposeService `yaml:"services"`
}

type ComposeService struct {
	Image       string      `yaml:"image"`
	Environment []string    `yaml:"environment,omitempty"`
	Ports       []string    `yaml:"ports,omitempty"`
	Volumes     []string    `yaml:"volumes,omitempty"`
	HealthCheck HealthCheck `yaml:"healthcheck,omitempty"`
}

type HealthCheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
}

func RunDockerCompose(repoDir string) error {
	// Retrieve the service names from the .env file
	serviceNames := os.Getenv("SERVICE_NAMES")
	if serviceNames == "" {
		serviceNames = "web"
	}
	log.Printf("Services to deploy: %s", serviceNames)

	// Split the service names into a slice
	servicesToDeploy := splitServiceNames(serviceNames)

	composePath := filepath.Join(repoDir, "docker-compose.yaml")
	composeConfig, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("failed to read compose file: %w", err)
	}
	log.Printf("Read compose file from %s", composePath)

	var composeFile ComposeFile
	err = yaml.Unmarshal(composeConfig, &composeFile)
	if err != nil {
		return fmt.Errorf("failed to parse docker-compose file: %w", err)
	}
	log.Printf("Parsed docker-compose.yaml successfully")

	dockerSock := os.Getenv("DOCKER_SOCK")
	if dockerSock == "" {
		dockerSock = "unix:///var/run/docker.sock"
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerSock),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}
	log.Printf("Created Docker client")

	for _, serviceName := range servicesToDeploy {
		service, exists := composeFile.Services[serviceName]
		if !exists {
			log.Printf("Service %s not defined in docker-compose.yaml, skipping", serviceName)
			continue
		}

		log.Printf("Pulling image for service: %s", serviceName)
		err := pullImage(cli, service.Image)
		if err != nil {
			return fmt.Errorf("failed to pull image for service %s: %w", serviceName, err)
		}

		isFirstDeployment := !areContainersRunning(cli, serviceName)
		err = deployService(cli, serviceName, repoDir, service, 1)
		if err != nil {
			return fmt.Errorf("failed to deploy service: %w", err)
		}

		if isFirstDeployment {
			log.Printf("Deployed %s for the first time", serviceName)
		} else {
			log.Printf("Updated %s with new container", serviceName)
		}
	}

	return nil
}

// Utility function to split service names
func splitServiceNames(serviceNames string) []string {
	return strings.Split(serviceNames, ",")
}

func pullImage(cli *client.Client, image string) error {
	ctx := context.Background()

	log.Printf("Attempting to pull image: %s", image)

	// Check if Docker registry credentials are provided
	username := os.Getenv("DOCKER_USERNAME")
	password := os.Getenv("DOCKER_PASSWORD")
	registry := os.Getenv("DOCKER_REGISTRY")

	var authConfig types.AuthConfig
	var authStr string
	if username != "" && password != "" {
		authConfig = types.AuthConfig{
			Username:      username,
			Password:      password,
			ServerAddress: registry,
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			log.Printf("Failed to encode auth config: %v", err)
			return err
		}
		authStr = base64.URLEncoding.EncodeToString(encodedJSON)
	}

	options := types.ImagePullOptions{}
	if authStr != "" {
		options.RegistryAuth = authStr
	}

	out, err := cli.ImagePull(ctx, image, options)
	if err != nil {
		log.Printf("Error pulling image %s: %v", image, err)
		return err
	}
	defer out.Close()

	// Read the output to ensure the image is pulled
	buf := make([]byte, 8)
	for {
		_, err := out.Read(buf)
		if err != nil {
			break
		}
	}

	log.Printf("Successfully pulled image: %s", image)
	return nil
}

func areContainersRunning(cli *client.Client, serviceName string) bool {
	ctx := context.Background()
	filter := filters.NewArgs()
	filter.Add("name", serviceName)

	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Filters: filter})
	if err != nil {
		log.Printf("Failed to list containers: %v", err)
		return false
	}

	return len(containers) > 0
}

func isImageTagDifferent(cli *client.Client, serviceName, imageTag string) bool {
	ctx := context.Background()
	filter := filters.NewArgs()
	filter.Add("name", serviceName)

	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Filters: filter})
	if err != nil {
		log.Printf("Failed to list containers: %v", err)
		return false
	}

	for _, container := range containers {
		log.Printf("Found existing container %s with image tag: %s", container.ID, container.Image)
		log.Printf("New image tag: %s", imageTag)
		if container.Image != imageTag {
			return true
		}
	}

	return false
}

func deployService(cli *client.Client, serviceName, repoDir string, service ComposeService, scale int) error {
	ctx := context.Background()

	// List existing containers
	existingContainers, err := cli.ContainerList(ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}
	log.Printf("Found %d existing containers", len(existingContainers))

	latestImageTag := service.Image
	containersToRemove := []string{}

	// Inspect existing containers and determine which to remove
	for _, container := range existingContainers {
		if !containsServiceName(container.Names, serviceName) {
			continue
		}

		log.Printf("Found existing container %s with image tag: %s", container.ID, container.Image)
		log.Printf("New image tag: %s", latestImageTag)

		containerInfo, err := cli.ContainerInspect(ctx, container.ID)
		if err != nil {
			return fmt.Errorf("failed to inspect container: %w", err)
		}

		if container.Image != latestImageTag {
			log.Printf("Preparing to remove existing container %s with outdated image tag", container.ID)
			containersToRemove = append(containersToRemove, container.ID)
		} else if containerInfo.State.Health != nil && containerInfo.State.Health.Status != "healthy" {
			log.Printf("Waiting for health check to complete for container %s", container.ID)
			err := waitForHealthCheck(ctx, cli, container.ID)
			if err != nil {
				return fmt.Errorf("health check failed for container %s: %w", container.ID, err)
			}
		} else {
			log.Printf("Existing container %s has the same image tag, no action needed", container.ID)
		}
	}

	// Create and start new containers
	if len(containersToRemove) != 0 {
		log.Printf("Creating and starting new containers for service: %s", serviceName)
		for i := 0; i < scale; i++ {
			instanceName := fmt.Sprintf("%s_%d_%d", serviceName, i, time.Now().UnixNano())
			err := createAndStartContainer(ctx, cli, instanceName, repoDir, service)
			if err != nil {
				return err
			}
			log.Printf("Waiting for health check to complete for new container %s", instanceName)
			err = waitForHealthCheck(ctx, cli, instanceName)
			if err != nil {
				return fmt.Errorf("health check failed for new container %s: %w", instanceName, err)
			}
			log.Printf("New container %s created and started successfully", instanceName)
		}

		for _, containerID := range containersToRemove {
			log.Printf("Removing container %s", containerID)
			err := cli.ContainerRemove(ctx, containerID, types.ContainerRemoveOptions{Force: true})
			if err != nil {
				return fmt.Errorf("failed to remove container: %w", err)
			}
		}
	} else {
		log.Printf("No existing containers with outdated image tags found, no action needed")
	}

	return nil
}

// Utility function to check if container name matches the service name
func containsServiceName(names []string, serviceName string) bool {
	for _, name := range names {
		if strings.Contains(name, serviceName) {
			return true
		}
	}
	return false
}

func waitForHealthCheck(ctx context.Context, cli *client.Client, containerID string) error {
	timeout := time.After(90 * time.Second)
	tick := time.Tick(3 * time.Second)

	// Add a short delay to allow Docker to register the health check
	time.Sleep(5 * time.Second)

	for {
		select {
		case <-timeout:
			return fmt.Errorf("health check timeout for container %s", containerID)
		case <-tick:
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

func createAndStartContainer(ctx context.Context, cli *client.Client, name, repoDir string, service ComposeService) error {
	log.Printf("Creating container %s with image %s", name, service.Image)

	containerConfig := &container.Config{
		Image: service.Image,
		Env:   service.Environment,
	}

	// Add health check if it's defined
	if service.HealthCheck.Test != nil {
		containerConfig.Healthcheck = &container.HealthConfig{
			Test:        service.HealthCheck.Test,
			Interval:    parseDuration(service.HealthCheck.Interval),
			Timeout:     parseDuration(service.HealthCheck.Timeout),
			Retries:     service.HealthCheck.Retries,
			StartPeriod: parseDuration(service.HealthCheck.StartPeriod),
		}
	}

	hostConfig := &container.HostConfig{
		PortBindings: mapPorts(service.Ports),
		Binds:        service.Volumes,
	}

	networkConfig := &network.NetworkingConfig{}

	log.Printf("Creating container with config: %+v, hostConfig: %+v", containerConfig, hostConfig)

	ctxWithTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := cli.ContainerCreate(ctxWithTimeout, containerConfig, hostConfig, networkConfig, nil, name)
	if err != nil {
		log.Printf("Failed to create container: %v", err)
		return fmt.Errorf("failed to create container: %w", err)
	}

	log.Printf("Container created successfully with ID: %s", resp.ID)

	log.Printf("Starting container %s", resp.ID)
	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel()

	if err := cli.ContainerStart(startCtx, resp.ID, types.ContainerStartOptions{}); err != nil {
		log.Printf("Failed to start container: %v", err)
		return fmt.Errorf("failed to start container: %w", err)
	}

	log.Printf("Container started successfully with ID: %s", resp.ID)

	return nil
}

func mapPorts(ports []string) nat.PortMap {
	portMap := nat.PortMap{}
	for _, port := range ports {
		hostPort, containerPort, _ := net.SplitHostPort(port)
		portMap[nat.Port(containerPort+"/tcp")] = []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: hostPort,
			},
		}
	}
	return portMap
}

func parseDuration(duration string) time.Duration {
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
