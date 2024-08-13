package network

import (
	"context"
	"log"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

type ComposeNetwork struct {
	Driver     string            `yaml:"driver,omitempty"`
	DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
}

func CreateNetwork(cli *client.Client, name string, config ComposeNetwork) error {
	ctx := context.Background()

	// Check if the network already exists
	existingNetworks, err := cli.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		log.Printf("Failed to list networks: %v", err)
		return err
	}

	for _, net := range existingNetworks {
		if net.Name == name {
			log.Printf("Network %s already exists, skipping creation", name)
			return nil
		}
	}

	networkCreate := types.NetworkCreate{
		Driver:  config.Driver,
		Options: config.DriverOpts,
	}

	_, err = cli.NetworkCreate(ctx, name, networkCreate)
	if err != nil {
		log.Printf("Failed to create network %s: %v", name, err)
		return err
	}

	log.Printf("Successfully created network: %s", name)
	return nil
}
