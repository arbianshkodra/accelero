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

// CreateNetworkForStack creates a compose-declared network and tags it with
// the accelero management labels so /networks and other stack-scoped
// introspection endpoints can find it.  Skips creation when a network of
// the same name already exists (idempotent deploys). Pre-existing
// unlabelled networks from older accelero versions stay unlabelled —
// Docker doesn't allow adding labels to a live network without a recreate,
// which would break every container on the network.
func CreateNetworkForStack(ctx context.Context, cli *client.Client, stackName, name string, config ComposeNetwork) error {
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

	labels := map[string]string{
		"managed-by":     "accelero",
		"accelero-stack": stackName,
	}

	opts := client.NetworkCreateOptions{
		Driver:  config.Driver,
		Options: config.DriverOpts,
		Labels:  labels,
	}

	if _, err := cli.NetworkCreate(ctx, name, opts); err != nil {
		return fmt.Errorf("failed to create network %s: %w", name, err)
	}

	logrus.Infof("Successfully created network: %s", name)
	return nil
}

// CreateNetworkWithContext is kept for backward compatibility with any
// out-of-tree callers; new code should use CreateNetworkForStack so the
// resulting network is labelled. Networks created by this overload do
// NOT carry accelero labels and will be invisible to /networks.
//
// Deprecated: use CreateNetworkForStack.
func CreateNetworkWithContext(ctx context.Context, cli *client.Client, name string, config ComposeNetwork) error {
	return CreateNetworkForStack(ctx, cli, "", name, config)
}
