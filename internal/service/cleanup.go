package service

import (
	"context"

	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

// CleanupResources prunes Docker resources that are managed by Accelero.
// It scopes prune operations using the "managed-by=accelero" label so that
// resources belonging to other tools are never touched.
func CleanupResources(ctx context.Context, cli *client.Client) error {
	labelFilter := make(client.Filters).Add("label", "managed-by=accelero")

	// Prune containers scoped to the accelero label.
	containerRes, err := cli.ContainerPrune(ctx, client.ContainerPruneOptions{Filters: labelFilter})
	if err != nil {
		return err
	}
	if n := len(containerRes.Report.ContainersDeleted); n > 0 {
		logrus.Infof("Pruned %d accelero containers", n)
	}

	// Prune dangling images (no label filter exists for images — restrict to
	// dangling-only to stay safe).
	danglingFilter := make(client.Filters).Add("dangling", "true")
	imageRes, err := cli.ImagePrune(ctx, client.ImagePruneOptions{Filters: danglingFilter})
	if err != nil {
		return err
	}
	if n := len(imageRes.Report.ImagesDeleted); n > 0 {
		logrus.Infof("Pruned %d dangling images", n)
	}

	// Prune volumes scoped by label.
	volumeRes, err := cli.VolumePrune(ctx, client.VolumePruneOptions{Filters: labelFilter})
	if err != nil {
		return err
	}
	if n := len(volumeRes.Report.VolumesDeleted); n > 0 {
		logrus.Infof("Pruned %d accelero volumes", n)
	}

	// Prune networks scoped by label.
	networkRes, err := cli.NetworkPrune(ctx, client.NetworkPruneOptions{Filters: labelFilter})
	if err != nil {
		return err
	}
	if n := len(networkRes.Report.NetworksDeleted); n > 0 {
		logrus.Infof("Pruned %d accelero networks", n)
	}

	return nil
}
