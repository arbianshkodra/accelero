package service

import (
	"context"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

func CleanupResources(cli *client.Client) error {
	ctx := context.Background()

	// Prune containers
	containerPruneFilters := filters.NewArgs()
	containerPruneReport, err := cli.ContainersPrune(ctx, containerPruneFilters)
	if err != nil {
		return err
	}
	logrus.Infof("Pruned containers: %v", containerPruneReport.ContainersDeleted)

	// Prune images
	imagePruneFilters := filters.NewArgs()
	imagePruneReport, err := cli.ImagesPrune(ctx, imagePruneFilters)
	if err != nil {
		return err
	}
	logrus.Infof("Pruned images: %v", imagePruneReport.ImagesDeleted)

	// Prune volumes
	volumePruneFilters := filters.NewArgs()
	volumePruneReport, err := cli.VolumesPrune(ctx, volumePruneFilters)
	if err != nil {
		return err
	}
	logrus.Infof("Pruned volumes: %v", volumePruneReport.VolumesDeleted)

	// Prune networks
	networkPruneFilters := filters.NewArgs()
	networkPruneReport, err := cli.NetworksPrune(ctx, networkPruneFilters)
	if err != nil {
		return err
	}
	logrus.Infof("Pruned networks: %v", networkPruneReport.NetworksDeleted)

	return nil
}
