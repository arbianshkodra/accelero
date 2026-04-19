package network

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
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
	existing, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list networks: %w", err)
	}

	for _, net := range existing.Items {
		if net.Name == name {
			logrus.Warnf("Network %s already exists, skipping creation", name)
			return nil
		}
	}

	opts := client.NetworkCreateOptions{
		Driver:  config.Driver,
		Options: config.DriverOpts,
	}

	if _, err := cli.NetworkCreate(ctx, name, opts); err != nil {
		return fmt.Errorf("failed to create network %s: %w", name, err)
	}

	logrus.Infof("Successfully created network: %s", name)
	return nil
}
