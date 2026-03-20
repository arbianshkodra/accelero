package service

import (
	"context"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// CleanupResources prunes Docker resources that are managed by Accelero.
// It scopes prune operations using the "managed-by=accelero" label so that
// resources belonging to other tools are never touched.
func CleanupResources(ctx context.Context, cli *client.Client) error {
	labelFilter := filters.NewArgs()
	labelFilter.Add("label", "managed-by=accelero")

	// Prune containers
	containerReport, err := cli.ContainersPrune(ctx, labelFilter)
	if err != nil {
		return err
	}
	if len(containerReport.ContainersDeleted) > 0 {
		logrus.Infof("Pruned %d accelero containers", len(containerReport.ContainersDeleted))
	}

	// Prune images (dangling only — no label filter available for images,
	// but we only prune dangling ones to stay safe)
	danglingFilter := filters.NewArgs()
	danglingFilter.Add("dangling", "true")
	imageReport, err := cli.ImagesPrune(ctx, danglingFilter)
	if err != nil {
		return err
	}
	if len(imageReport.ImagesDeleted) > 0 {
		logrus.Infof("Pruned %d dangling images", len(imageReport.ImagesDeleted))
	}

	// Prune volumes (scoped by label)
	volumeReport, err := cli.VolumesPrune(ctx, labelFilter)
	if err != nil {
		return err
	}
	if len(volumeReport.VolumesDeleted) > 0 {
		logrus.Infof("Pruned %d accelero volumes", len(volumeReport.VolumesDeleted))
	}

	// Prune networks (scoped by label)
	networkReport, err := cli.NetworksPrune(ctx, labelFilter)
	if err != nil {
		return err
	}
	if len(networkReport.NetworksDeleted) > 0 {
		logrus.Infof("Pruned %d accelero networks", len(networkReport.NetworksDeleted))
	}

	return nil
}
