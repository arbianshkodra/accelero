package service

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/sirupsen/logrus"
)

type ComposeService struct {
	Image       string            `yaml:"image"`
	Environment EnvVars           `yaml:"environment,omitempty"`
	EnvFile     []string          `yaml:"env_file,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Command     []string          `yaml:"command,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	HealthCheck HealthCheck       `yaml:"healthcheck,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
	Restart     string            `yaml:"restart,omitempty"`
	MemLimit    string            `yaml:"mem_limit,omitempty"`
	CPULimit    string            `yaml:"cpu_limit,omitempty"`
}
type DockerClient interface {
	ContainerList(ctx context.Context, options types.ContainerListOptions) ([]types.Container, error)
}

type EnvVars []string

func (e *EnvVars) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw []string
	if err := unmarshal(&raw); err == nil {
		*e = raw
		return nil
	}

	var rawMap map[string]string
	if err := unmarshal(&rawMap); err == nil {
		for k, v := range rawMap {
			*e = append(*e, fmt.Sprintf("%s=%s", k, v))
		}
		return nil
	}

	return fmt.Errorf("failed to unmarshal environment variables")
}

type HealthCheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
}

func ParseDuration(duration string) time.Duration {
	if duration == "" {
		return 0
	}
	parsedDuration, err := time.ParseDuration(duration)
	if err != nil {
		logrus.Warnf("Failed to parse duration '%s', using default 0", duration)
		return 0
	}
	return parsedDuration
}
