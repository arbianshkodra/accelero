package compose

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/arbianshkodra/accelero/internal/network"
	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/arbianshkodra/accelero/internal/utils"

	"github.com/docker/docker/client"
	"gopkg.in/yaml.v2"
)

type ComposeFile struct {
	Version  string                            `yaml:"version"`
	Services map[string]service.ComposeService `yaml:"services"`
	Networks map[string]network.ComposeNetwork `yaml:"networks,omitempty"`
}

func RunDockerCompose(repoDir string) error {
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

	// Retrieve the service names from the .env file
	serviceNames := os.Getenv("SERVICE_NAMES")
	if serviceNames == "" {
		serviceMap := make(map[string]interface{})
		for k, v := range composeFile.Services {
			serviceMap[k] = v
		}
		serviceNames = strings.Join(utils.GetAllServices(serviceMap), ",")
	}
	log.Printf("Services to deploy: %s", serviceNames)

	servicesToDeploy := utils.SplitServiceNames(serviceNames)

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

	// Create networks
	for netName, netConfig := range composeFile.Networks {
		log.Printf("Creating network %s with config %+v", netName, netConfig)
		err := network.CreateNetwork(cli, netName, netConfig)
		if err != nil {
			return fmt.Errorf("failed to create network %s: %w", netName, err)
		}
	}

	for _, serviceName := range servicesToDeploy {
		svc, exists := composeFile.Services[serviceName]
		if !exists {
			log.Printf("Service %s not defined in docker-compose.yaml, skipping", serviceName)
			continue
		}

		// Deploy dependencies first
		for _, dependency := range svc.DependsOn {
			dependentService, exists := composeFile.Services[dependency]
			if !exists {
				log.Printf("Dependent service %s not defined in docker-compose.yaml, skipping", dependency)
				continue
			}

			log.Printf("Deploying dependent service: %s", dependency)
			err := service.DeployService(cli, dependency, repoDir, dependentService, 1)
			if err != nil {
				return fmt.Errorf("failed to deploy dependent service %s: %w", dependency, err)
			}
		}

		log.Printf("Pulling image for service: %s", serviceName)
		err := service.PullImage(cli, svc.Image)
		if err != nil {
			return fmt.Errorf("failed to pull image for service %s: %w", serviceName, err)
		}

		isFirstDeployment := !service.AreContainersRunning(cli, serviceName)
		err = service.DeployService(cli, serviceName, repoDir, svc, 1)
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
