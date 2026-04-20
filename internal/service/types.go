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
	Ports       Ports             `yaml:"ports,omitempty"`
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
	DependsOn   Dependencies      `yaml:"depends_on,omitempty"`
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
	Logging         *LoggingConfig    `yaml:"logging,omitempty"`
	Deploy          DeployConfig      `yaml:"deploy,omitempty"`
}

// DeployConfig mirrors docker-compose's `deploy:` subtree. Only Replicas is
// acted on today; Mode / Resources / UpdateConfig / RollbackConfig etc.
// parse but do not change runtime behaviour. The `replicas` field works in
// non-swarm mode for Accelero because we're not using Docker's swarm stack
// deployer — we implement the rolling replica strategy ourselves.
type DeployConfig struct {
	Replicas *int   `yaml:"replicas,omitempty"`
	Mode     string `yaml:"mode,omitempty"` // "replicated" | "global"; currently informational
}

// DesiredReplicas returns the effective replica count for a service.
// Missing or <= 0 values fall back to 1, matching docker-compose semantics.
func (s ComposeService) DesiredReplicas() int {
	if s.Deploy.Replicas == nil || *s.Deploy.Replicas <= 0 {
		return 1
	}
	return *s.Deploy.Replicas
}

// HasStaticPublishedPort reports whether any port entry binds a fixed host
// port. Used to reject replicas > 1 with static port publishing, since N
// replicas on the same host port would collide on container create.
func (s ComposeService) HasStaticPublishedPort() bool {
	for _, p := range s.Ports {
		// Strip protocol suffix: "8080:80/tcp" -> "8080:80"
		base := p
		if i := strings.Index(base, "/"); i >= 0 {
			base = base[:i]
		}
		// A bare "80" or "0" is exposed-only, not published — skip.
		// Anything with a colon has an explicit host port.
		if strings.Contains(base, ":") {
			return true
		}
	}
	return false
}

// LoggingConfig maps onto container.HostConfig.LogConfig.
//
//	logging:
//	  driver: json-file
//	  options:
//	    max-size: "10m"
//	    max-file: "3"
type LoggingConfig struct {
	Driver  string            `yaml:"driver"`
	Options map[string]string `yaml:"options,omitempty"`
}

// Ports accepts both the short string form and the long map form.  The long
// form is normalised into the canonical "[host_ip:]host_port:container_port[/proto]"
// string so downstream helpers (utils.MapPorts) only see one shape.
type Ports []string

type longPort struct {
	Target    int    `yaml:"target"`
	Published any    `yaml:"published"` // int OR string (range); we accept int
	Protocol  string `yaml:"protocol,omitempty"`
	HostIP    string `yaml:"host_ip,omitempty"`
	// Mode (ingress|host) is swarm-only; parsed-and-ignored.
	Mode string `yaml:"mode,omitempty"`
}

func (p *Ports) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw []any
	if err := unmarshal(&raw); err != nil {
		return fmt.Errorf("ports must be a list: %w", err)
	}

	for _, entry := range raw {
		switch v := entry.(type) {
		case string:
			*p = append(*p, v)
		case int:
			// Bare container port, e.g. `- 80`.
			*p = append(*p, fmt.Sprintf("%d", v))
		case map[any]any:
			// Long form.  Re-serialize + re-parse through longPort so we get
			// typed fields for free (yaml.v2 hands us map[any]any at the
			// interface{} boundary).
			var lp longPort
			// Manual extraction is simpler than round-tripping.
			if tgt, ok := v["target"]; ok {
				if n, ok := tgt.(int); ok {
					lp.Target = n
				} else {
					return fmt.Errorf("ports: target must be int, got %T", tgt)
				}
			}
			if pub, ok := v["published"]; ok {
				lp.Published = pub
			}
			if proto, ok := v["protocol"]; ok {
				if s, ok := proto.(string); ok {
					lp.Protocol = s
				}
			}
			if ip, ok := v["host_ip"]; ok {
				if s, ok := ip.(string); ok {
					lp.HostIP = s
				}
			}
			s, err := lp.toShortForm()
			if err != nil {
				return fmt.Errorf("ports: %w", err)
			}
			*p = append(*p, s)
		default:
			return fmt.Errorf("ports entry must be a string, int, or map, got %T", entry)
		}
	}
	return nil
}

func (l longPort) toShortForm() (string, error) {
	if l.Target == 0 {
		return "", fmt.Errorf("target is required")
	}
	var b strings.Builder
	if l.HostIP != "" {
		b.WriteString(l.HostIP)
		b.WriteByte(':')
	}
	switch v := l.Published.(type) {
	case nil:
		// No published port — container-only exposure.
	case int:
		if v != 0 {
			b.WriteString(fmt.Sprintf("%d", v))
		}
		b.WriteByte(':')
	case string:
		if v != "" {
			b.WriteString(v)
		}
		b.WriteByte(':')
	default:
		return "", fmt.Errorf("published must be int or string, got %T", l.Published)
	}
	// When `published` wasn't written, we still need the separator if there's
	// a host_ip (otherwise the parser in utils would treat host_ip as the host
	// port).  Guard: if we only have host_ip + target, drop the host_ip since
	// compose's contract is that published is required in that case.  This
	// keeps parsing unambiguous.
	if l.HostIP != "" && l.Published == nil {
		return "", fmt.Errorf("host_ip requires published to be set")
	}
	b.WriteString(fmt.Sprintf("%d", l.Target))
	if l.Protocol != "" {
		b.WriteByte('/')
		b.WriteString(l.Protocol)
	}
	return b.String(), nil
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

// Dependency condition constants, matching docker-compose's vocabulary.
const (
	DependencyConditionStarted           = "service_started"
	DependencyConditionHealthy           = "service_healthy"
	DependencyConditionCompletedOK       = "service_completed_successfully"
)

// DependencyConfig holds the per-dependency long-form options.
// Only Condition is acted on today; Required and Restart are parsed for
// forward-compat but currently do not change deploy behaviour.
type DependencyConfig struct {
	Condition string `yaml:"condition,omitempty"`
	Required  *bool  `yaml:"required,omitempty"`
	Restart   bool   `yaml:"restart,omitempty"`
}

// Dependencies accepts both compose forms:
//
//	depends_on:
//	  - db
//	  - redis
//
//	depends_on:
//	  db:
//	    condition: service_healthy
//	  redis:
//	    condition: service_started
//
// The short form is normalised to a map with the default condition
// (service_started), so downstream consumers only see one shape.
type Dependencies map[string]DependencyConfig

func (d *Dependencies) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try the short-form list first.
	var list []string
	if err := unmarshal(&list); err == nil {
		*d = make(Dependencies, len(list))
		for _, name := range list {
			(*d)[name] = DependencyConfig{Condition: DependencyConditionStarted}
		}
		return nil
	}

	// Try the long-form map.
	var raw map[string]DependencyConfig
	if err := unmarshal(&raw); err == nil {
		out := make(Dependencies, len(raw))
		for name, cfg := range raw {
			if cfg.Condition == "" {
				cfg.Condition = DependencyConditionStarted
			}
			out[name] = cfg
		}
		*d = out
		return nil
	}

	return fmt.Errorf("depends_on must be a list of service names or a map of name -> config")
}

// Names returns the dependency names in deterministic (sorted) order so
// topological-sort output is reproducible across runs.
func (d Dependencies) Names() []string {
	names := make([]string, 0, len(d))
	for name := range d {
		names = append(names, name)
	}
	// Sort for determinism.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
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
