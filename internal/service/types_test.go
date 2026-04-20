package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

// wrap embeds the target type under `x:` so yaml.v2's unmarshaler can invoke
// the custom UnmarshalYAML correctly on a single value.
type wrap[T any] struct {
	X T `yaml:"x"`
}

func decode[T any](t *testing.T, yamlText string) T {
	t.Helper()
	var w wrap[T]
	require.NoErrorf(t, yaml.Unmarshal([]byte(yamlText), &w), "yaml: %q", yamlText)
	return w.X
}

func TestCommand_StringOrList(t *testing.T) {
	assert.Equal(t, Command{"echo", "hello"}, decode[Command](t, `x: "echo hello"`))
	assert.Equal(t, Command{"sh", "-c", "echo hi"}, decode[Command](t, `
x:
  - sh
  - -c
  - echo hi
`))
}

func TestEntrypoint_UsesCommandType(t *testing.T) {
	// Entrypoint shares the Command unmarshaler; verify the wiring here.
	assert.Equal(t, Command{"/entrypoint.sh"}, decode[Command](t, `x: /entrypoint.sh`))
}

func TestStringList_SingleOrList(t *testing.T) {
	assert.Equal(t, StringList{"8.8.8.8"}, decode[StringList](t, `x: 8.8.8.8`))
	assert.Equal(t, StringList{"8.8.8.8", "8.8.4.4"}, decode[StringList](t, `
x:
  - 8.8.8.8
  - 8.8.4.4
`))
	assert.Equal(t, StringList(nil), decode[StringList](t, `x: ""`))
}

func TestExposeList_StringAndInt(t *testing.T) {
	assert.Equal(t, ExposeList{"3000", "8080/tcp", "9090"}, decode[ExposeList](t, `
x:
  - "3000"
  - "8080/tcp"
  - 9090
`))
}

func TestExtraHosts_ListAndMap(t *testing.T) {
	assert.Equal(t, ExtraHosts{"host.example:10.0.0.1", "other:10.0.0.2"}, decode[ExtraHosts](t, `
x:
  - "host.example:10.0.0.1"
  - "other:10.0.0.2"
`))

	got := decode[ExtraHosts](t, `
x:
  host.example: 10.0.0.1
  other: 10.0.0.2
`)
	// Map iteration order is not stable, so compare as a set.
	assert.ElementsMatch(t, []string{"host.example:10.0.0.1", "other:10.0.0.2"}, got)
}

func TestTmpfs_StringListAndMap(t *testing.T) {
	assert.Equal(t, Tmpfs{"/run": ""}, decode[Tmpfs](t, `x: /run`))
	assert.Equal(t, Tmpfs{"/run": "", "/tmp": ""}, decode[Tmpfs](t, `
x:
  - /run
  - /tmp
`))
	assert.Equal(t, Tmpfs{"/run": "size=64m", "/tmp": ""}, decode[Tmpfs](t, `
x:
  /run: size=64m
  /tmp: ""
`))
}

func TestDependencies_ShortForm(t *testing.T) {
	deps := decode[Dependencies](t, `
x:
  - db
  - redis
`)
	require.Len(t, deps, 2)
	assert.Equal(t, DependencyConfig{Condition: DependencyConditionStarted}, deps["db"])
	assert.Equal(t, DependencyConfig{Condition: DependencyConditionStarted}, deps["redis"])
}

func TestDependencies_LongForm(t *testing.T) {
	deps := decode[Dependencies](t, `
x:
  db:
    condition: service_healthy
  redis:
    condition: service_started
  migrate:
    condition: service_completed_successfully
    required: false
`)
	require.Len(t, deps, 3)
	assert.Equal(t, DependencyConditionHealthy, deps["db"].Condition)
	assert.Equal(t, DependencyConditionStarted, deps["redis"].Condition)
	assert.Equal(t, DependencyConditionCompletedOK, deps["migrate"].Condition)
	require.NotNil(t, deps["migrate"].Required)
	assert.False(t, *deps["migrate"].Required)
}

func TestDependencies_LongForm_DefaultCondition(t *testing.T) {
	// Entries without an explicit condition should fall back to service_started.
	deps := decode[Dependencies](t, `
x:
  db: {}
  redis:
    required: true
`)
	require.Len(t, deps, 2)
	assert.Equal(t, DependencyConditionStarted, deps["db"].Condition)
	assert.Equal(t, DependencyConditionStarted, deps["redis"].Condition)
}

func TestDependencies_NamesIsSorted(t *testing.T) {
	deps := Dependencies{
		"zulu":  {Condition: DependencyConditionStarted},
		"alpha": {Condition: DependencyConditionStarted},
		"mike":  {Condition: DependencyConditionStarted},
	}
	assert.Equal(t, []string{"alpha", "mike", "zulu"}, deps.Names())
}

func TestDependencies_InvalidForm(t *testing.T) {
	var w wrap[Dependencies]
	err := yaml.Unmarshal([]byte(`x: "just a string"`), &w)
	require.Error(t, err)
}

func TestComposeService_FullFieldRoundTrip(t *testing.T) {
	yamlText := `
image: nginx:1.27.0
entrypoint: /docker-entrypoint.sh
working_dir: /app
user: "1000:1000"
hostname: web-01
domainname: internal.example
expose:
  - "8080"
  - 9090
stop_grace_period: 30s
stop_signal: SIGTERM
pull_policy: missing
dns:
  - 1.1.1.1
  - 8.8.8.8
dns_search: local.example
extra_hosts:
  - "host.docker.internal:10.0.0.1"
cap_add:
  - SYS_PTRACE
cap_drop:
  - MKNOD
privileged: true
tmpfs:
  - /run
  - /tmp
shm_size: 256m
init: true
`
	var svc ComposeService
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &svc))

	assert.Equal(t, "nginx:1.27.0", svc.Image)
	assert.Equal(t, Command{"/docker-entrypoint.sh"}, svc.Entrypoint)
	assert.Equal(t, "/app", svc.WorkingDir)
	assert.Equal(t, "1000:1000", svc.User)
	assert.Equal(t, "web-01", svc.Hostname)
	assert.Equal(t, "internal.example", svc.Domainname)
	assert.Equal(t, ExposeList{"8080", "9090"}, svc.Expose)
	assert.Equal(t, "30s", svc.StopGracePeriod)
	assert.Equal(t, "SIGTERM", svc.StopSignal)
	assert.Equal(t, "missing", svc.PullPolicy)
	assert.Equal(t, StringList{"1.1.1.1", "8.8.8.8"}, svc.DNS)
	assert.Equal(t, StringList{"local.example"}, svc.DNSSearch)
	assert.Equal(t, ExtraHosts{"host.docker.internal:10.0.0.1"}, svc.ExtraHosts)
	assert.Equal(t, []string{"SYS_PTRACE"}, svc.CapAdd)
	assert.Equal(t, []string{"MKNOD"}, svc.CapDrop)
	assert.True(t, svc.Privileged)
	assert.Equal(t, Tmpfs{"/run": "", "/tmp": ""}, svc.Tmpfs)
	assert.Equal(t, "256m", svc.ShmSize)
	require.NotNil(t, svc.Init)
	assert.True(t, *svc.Init)
}
