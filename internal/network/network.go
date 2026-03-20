package network

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type ComposeNetwork struct {
	Driver     string            `yaml:"driver,omitempty"`
	DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
}

func CreateNetwork(cli *client.Client, name string, config ComposeNetwork) error {
	return CreateNetworkWithContext(context.Background(), cli, name, config)
}

func CreateNetworkWithContext(ctx context.Context, cli *client.Client, name string, config ComposeNetwork) error {
	existingNetworks, err := cli.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list networks: %w", err)
	}

	for _, net := range existingNetworks {
		if net.Name == name {
			logrus.Warnf("Network %s already exists, skipping creation", name)
			return nil
		}
	}

	networkCreate := types.NetworkCreate{
		Driver:  config.Driver,
		Options: config.DriverOpts,
	}

	if _, err := cli.NetworkCreate(ctx, name, networkCreate); err != nil {
		return fmt.Errorf("failed to create network %s: %w", name, err)
	}

	logrus.Infof("Successfully created network: %s", name)
	return nil
}
