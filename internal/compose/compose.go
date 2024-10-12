package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arbianshkodra/accelero/internal/network"
	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/arbianshkodra/accelero/internal/utils"

	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type ComposeFile struct {
	Version  string                            `yaml:"version"`
	Services map[string]service.ComposeService `yaml:"services"`
	Networks map[string]network.ComposeNetwork `yaml:"networks,omitempty"`
}

func RunDockerCompose(ctx context.Context, repoDir string) error {
	composePath := filepath.Join(repoDir, "docker-compose.yaml")
	composeConfig, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("failed to read compose file: %w", err)
	}
	logrus.Infof("Read compose file from %s", composePath)

	var composeFile ComposeFile
	err = yaml.Unmarshal(composeConfig, &composeFile)
	if err != nil {
		return fmt.Errorf("failed to parse docker-compose file: %w", err)
	}
	logrus.Info("Parsed docker-compose.yaml successfully")

	// Retrieve the service names from the environment variable
	serviceNames := os.Getenv("SERVICE_NAMES")
	if serviceNames == "" {
		serviceMap := make(map[string]interface{})
		for k, v := range composeFile.Services {
			serviceMap[k] = v
		}
		serviceNames = strings.Join(utils.GetAllServices(serviceMap), ",")
	}
	logrus.Infof("Services to deploy: %s", serviceNames)

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
	logrus.Info("Created Docker client")

	// Create networks
	for netName, netConfig := range composeFile.Networks {
		logrus.Infof("Creating network %s with config %+v", netName, netConfig)
		if err := network.CreateNetwork(cli, netName, netConfig); err != nil {
			return fmt.Errorf("failed to create network %s: %w", netName, err)
		}
	}

	for _, serviceName := range servicesToDeploy {
		svc, exists := composeFile.Services[serviceName]
		if !exists {
			logrus.Warnf("Service %s not defined in docker-compose.yaml, skipping", serviceName)
			continue
		}

		// Deploy dependencies first
		for _, dependency := range svc.DependsOn {
			dependentService, exists := composeFile.Services[dependency]
			if !exists {
				logrus.Warnf("Dependent service %s not defined in docker-compose.yaml, skipping", dependency)
				continue
			}

			logrus.Infof("Deploying dependent service: %s", dependency)
			if err := service.DeployService(cli, dependency, repoDir, dependentService, 1); err != nil {
				return fmt.Errorf("failed to deploy dependent service %s: %w", dependency, err)
			}
		}

		logrus.Infof("Pulling image for service: %s", serviceName)
		if err := service.PullImage(cli, svc.Image); err != nil {
			return fmt.Errorf("failed to pull image for service %s: %w", serviceName, err)
		}

		isFirstDeployment, err := service.AreContainersRunning(cli, serviceName)
		if err != nil {
			return fmt.Errorf("failed to check if containers are running for service %s: %w", serviceName, err)
		}

		if err := service.DeployService(cli, serviceName, repoDir, svc, 1); err != nil {
			return fmt.Errorf("failed to deploy service %s: %w", serviceName, err)
		}

		if !isFirstDeployment {
			logrus.Infof("Deployed %s for the first time", serviceName)
		} else {
			logrus.Infof("Updated %s with new container", serviceName)
		}
	}

	return nil
}
