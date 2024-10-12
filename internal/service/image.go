package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// PullImage pulls a Docker image using the Docker client.
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
