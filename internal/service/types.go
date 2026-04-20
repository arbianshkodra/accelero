package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// ComposeService mirrors the service-level fields we support from a
// docker-compose.yaml.  Unsupported fields are silently ignored at unmarshal
// time, keeping the deployer forward-compatible with compose variations.
type ComposeService struct {
	Image       string            `yaml:"image"`
	Environment EnvVars           `yaml:"environment,omitempty"`
	EnvFile     []string          `yaml:"env_file,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Command     Command           `yaml:"command,omitempty"`
	Entrypoint  Command           `yaml:"entrypoint,omitempty"`
	WorkingDir  string            `yaml:"working_dir,omitempty"`
	User        string            `yaml:"user,omitempty"`
	Hostname    string            `yaml:"hostname,omitempty"`
	Domainname  string            `yaml:"domainname,omitempty"`
	Expose      ExposeList        `yaml:"expose,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	HealthCheck HealthCheck       `yaml:"healthcheck,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"`
	DependsOn   []string          `yaml:"depends_on,omitempty"`
	Restart     string            `yaml:"restart,omitempty"`
	MemLimit    string            `yaml:"mem_limit,omitempty"`
	CPULimit    string            `yaml:"cpu_limit,omitempty"`

	// Host-level fields
	DNS             StringList        `yaml:"dns,omitempty"`
	DNSSearch       StringList        `yaml:"dns_search,omitempty"`
	ExtraHosts      ExtraHosts        `yaml:"extra_hosts,omitempty"`
	CapAdd          []string          `yaml:"cap_add,omitempty"`
	CapDrop         []string          `yaml:"cap_drop,omitempty"`
	Privileged      bool              `yaml:"privileged,omitempty"`
	Tmpfs           Tmpfs             `yaml:"tmpfs,omitempty"`
	ShmSize         string            `yaml:"shm_size,omitempty"`
	Init            *bool             `yaml:"init,omitempty"`
	StopGracePeriod string            `yaml:"stop_grace_period,omitempty"`
	StopSignal      string            `yaml:"stop_signal,omitempty"`
	PullPolicy      string            `yaml:"pull_policy,omitempty"`
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

// Command supports both string and list forms for docker-compose `command`
// and `entrypoint`.
type Command []string

func (c *Command) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		*c = list
		return nil
	}

	var str string
	if err := unmarshal(&str); err == nil {
		// Split on spaces, matching docker-compose's behaviour for a bare
		// string.  Shell-style quoting is not handled.
		*c = strings.Fields(str)
		return nil
	}

	return fmt.Errorf("failed to unmarshal command/entrypoint: must be a string or list of strings")
}

// StringList accepts either a single string or a list of strings.  Used for
// fields where compose tolerates both shapes (dns, dns_search).
type StringList []string

func (s *StringList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		*s = list
		return nil
	}
	var str string
	if err := unmarshal(&str); err == nil {
		if str == "" {
			return nil
		}
		*s = []string{str}
		return nil
	}
	return fmt.Errorf("failed to unmarshal string list")
}

// ExposeList accepts entries as strings or ints and normalises them to
// strings; Docker's ExposedPorts map keys on `"port/proto"` form.
type ExposeList []string

func (e *ExposeList) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw []interface{}
	if err := unmarshal(&raw); err != nil {
		return fmt.Errorf("expose must be a list: %w", err)
	}
	for _, v := range raw {
		switch x := v.(type) {
		case string:
			*e = append(*e, x)
		case int:
			*e = append(*e, fmt.Sprintf("%d", x))
		default:
			return fmt.Errorf("expose entry must be a string or int, got %T", v)
		}
	}
	return nil
}

// ExtraHosts accepts the two compose forms and normalises to Docker's
// `host:ip` list.
type ExtraHosts []string

func (h *ExtraHosts) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		*h = list
		return nil
	}
	var m map[string]string
	if err := unmarshal(&m); err == nil {
		for host, ip := range m {
			*h = append(*h, fmt.Sprintf("%s:%s", host, ip))
		}
		return nil
	}
	return fmt.Errorf("extra_hosts must be a list of \"host:ip\" strings or a map")
}

// Tmpfs accepts a string, a list, or a map and normalises to Docker's
// `map[path]options` shape used by HostConfig.Tmpfs.
type Tmpfs map[string]string

func (t *Tmpfs) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if *t == nil {
		*t = make(map[string]string)
	}
	// single string
	var str string
	if err := unmarshal(&str); err == nil && str != "" {
		(*t)[str] = ""
		return nil
	}
	// list of strings
	var list []string
	if err := unmarshal(&list); err == nil {
		for _, path := range list {
			(*t)[path] = ""
		}
		return nil
	}
	// map path -> options
	var m map[string]string
	if err := unmarshal(&m); err == nil {
		for k, v := range m {
			(*t)[k] = v
		}
		return nil
	}
	return fmt.Errorf("tmpfs must be a string, list, or map")
}

type HealthCheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
}

// ParseDuration wraps time.ParseDuration and logs invalid values instead of
// returning an error, matching the forgiving behaviour docker-compose users
// expect for optional fields like healthcheck timings.
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
