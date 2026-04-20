package service

// ComposeVolume mirrors the entries allowed under the top-level `volumes:`
// section of a docker-compose file.
//
//	volumes:
//	  pg_data:                  # default driver, accelero-managed
//	  cache:
//	    driver: local
//	    driver_opts:
//	      type: tmpfs
//	      device: tmpfs
//	  shared_logs:
//	    external: true          # already exists, Accelero doesn't create/delete
//	    name: real_volume_name  # optional explicit name (bypasses scoping)
type ComposeVolume struct {
	Driver     string            `yaml:"driver,omitempty"`
	DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
	External   bool              `yaml:"external,omitempty"`
	Name       string            `yaml:"name,omitempty"`
	Labels     map[string]string `yaml:"labels,omitempty"`
}
