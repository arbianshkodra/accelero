package stack

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// matchesServiceName
// ---------------------------------------------------------------------------

func TestMatchesServiceName(t *testing.T) {
	tests := []struct {
		name          string
		containerName string
		serviceName   string
		want          bool
	}{
		{
			name:          "standard name with index and suffix",
			containerName: "/web_0_123",
			serviceName:   "web",
			want:          true,
		},
		{
			name:          "exact match with leading slash",
			containerName: "/web",
			serviceName:   "web",
			want:          true,
		},
		{
			name:          "different service with shared prefix",
			containerName: "/webproxy_0",
			serviceName:   "web",
			want:          false,
		},
		{
			name:          "different service with hyphenated prefix",
			containerName: "/my-web_1",
			serviceName:   "web",
			want:          false,
		},
		{
			name:          "no leading slash",
			containerName: "web_0",
			serviceName:   "web",
			want:          true,
		},
		{
			name:          "exact match no slash",
			containerName: "web",
			serviceName:   "web",
			want:          true,
		},
		{
			name:          "container name shorter than service name",
			containerName: "/we",
			serviceName:   "web",
			want:          false,
		},
		{
			name:          "empty container name",
			containerName: "",
			serviceName:   "web",
			want:          false,
		},
		{
			name:          "only slash",
			containerName: "/",
			serviceName:   "web",
			want:          false,
		},
		{
			name:          "service name with underscore",
			containerName: "/my_app_0_123",
			serviceName:   "my_app",
			want:          true,
		},
		{
			name:          "non-underscore separator after match",
			containerName: "/web-0",
			serviceName:   "web",
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesServiceName(tt.containerName, tt.serviceName)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// topologicalSort
// ---------------------------------------------------------------------------

func TestTopologicalSort_NoDependencies(t *testing.T) {
	services := map[string]service.ComposeService{
		"web": {Image: "nginx"},
		"api": {Image: "myapi"},
		"db":  {Image: "postgres"},
	}
	targets := []string{"web", "api", "db"}

	sorted, err := topologicalSort(targets, services)
	assert.NoError(t, err)
	assert.Len(t, sorted, 3)

	// All targets must be present.
	sortedCopy := make([]string, len(sorted))
	copy(sortedCopy, sorted)
	sort.Strings(sortedCopy)
	expectedSorted := []string{"api", "db", "web"}
	assert.Equal(t, expectedSorted, sortedCopy)
}

// deps builds a service.Dependencies out of a short list of service names
// using the default condition — keeps the topo-sort tests readable.
func deps(names ...string) service.Dependencies {
	out := make(service.Dependencies, len(names))
	for _, n := range names {
		out[n] = service.DependencyConfig{Condition: service.DependencyConditionStarted}
	}
	return out
}

func TestTopologicalSort_LinearChain(t *testing.T) {
	// A depends on B, B depends on C  =>  order should be C, B, A.
	services := map[string]service.ComposeService{
		"A": {Image: "a", DependsOn: deps("B")},
		"B": {Image: "b", DependsOn: deps("C")},
		"C": {Image: "c"},
	}
	targets := []string{"A", "B", "C"}

	sorted, err := topologicalSort(targets, services)
	assert.NoError(t, err)
	assert.Equal(t, []string{"C", "B", "A"}, sorted)
}

func TestTopologicalSort_Diamond(t *testing.T) {
	// A -> B, A -> C, B -> D, C -> D  =>  D first, A last.
	services := map[string]service.ComposeService{
		"A": {Image: "a", DependsOn: deps("B", "C")},
		"B": {Image: "b", DependsOn: deps("D")},
		"C": {Image: "c", DependsOn: deps("D")},
		"D": {Image: "d"},
	}
	targets := []string{"A", "B", "C", "D"}

	sorted, err := topologicalSort(targets, services)
	assert.NoError(t, err)
	assert.Len(t, sorted, 4)

	// D must come before B and C; B and C must come before A.
	indexOf := make(map[string]int)
	for i, s := range sorted {
		indexOf[s] = i
	}
	assert.Less(t, indexOf["D"], indexOf["B"])
	assert.Less(t, indexOf["D"], indexOf["C"])
	assert.Less(t, indexOf["B"], indexOf["A"])
	assert.Less(t, indexOf["C"], indexOf["A"])
}

func TestTopologicalSort_CircularDependency(t *testing.T) {
	services := map[string]service.ComposeService{
		"A": {Image: "a", DependsOn: deps("B")},
		"B": {Image: "b", DependsOn: deps("A")},
	}
	targets := []string{"A", "B"}

	_, err := topologicalSort(targets, services)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "circular dependency")
}

func TestTopologicalSort_TransitiveDepsIncluded(t *testing.T) {
	// Target is only A, but A -> B -> C, so B and C must also be included.
	services := map[string]service.ComposeService{
		"A": {Image: "a", DependsOn: deps("B")},
		"B": {Image: "b", DependsOn: deps("C")},
		"C": {Image: "c"},
	}
	targets := []string{"A"}

	sorted, err := topologicalSort(targets, services)
	assert.NoError(t, err)
	assert.Len(t, sorted, 3)
	assert.Equal(t, []string{"C", "B", "A"}, sorted)
}

// ---------------------------------------------------------------------------
// resolveServiceFilter
// ---------------------------------------------------------------------------

func TestResolveServiceFilter_EmptyFilter(t *testing.T) {
	services := map[string]service.ComposeService{
		"web": {Image: "nginx"},
		"api": {Image: "myapi"},
	}

	result := resolveServiceFilter("", services)
	assert.Len(t, result, 2)

	sort.Strings(result)
	assert.Equal(t, []string{"api", "web"}, result)
}

func TestResolveServiceFilter_SpecificServices(t *testing.T) {
	services := map[string]service.ComposeService{
		"web":    {Image: "nginx"},
		"api":    {Image: "myapi"},
		"worker": {Image: "worker"},
	}

	result := resolveServiceFilter("web,api", services)
	assert.Len(t, result, 2)

	sort.Strings(result)
	assert.Equal(t, []string{"api", "web"}, result)
}

func TestResolveServiceFilter_UnknownServiceSkipped(t *testing.T) {
	services := map[string]service.ComposeService{
		"web": {Image: "nginx"},
	}

	result := resolveServiceFilter("web,nonexistent", services)
	assert.Len(t, result, 1)
	assert.Equal(t, []string{"web"}, result)
}

func TestResolveServiceFilter_WhitespaceHandling(t *testing.T) {
	services := map[string]service.ComposeService{
		"web": {Image: "nginx"},
		"api": {Image: "myapi"},
	}

	result := resolveServiceFilter("  web , api  ", services)
	assert.Len(t, result, 2)

	sort.Strings(result)
	assert.Equal(t, []string{"api", "web"}, result)
}

func TestResolveServiceFilter_AllUnknown(t *testing.T) {
	services := map[string]service.ComposeService{
		"web": {Image: "nginx"},
	}

	result := resolveServiceFilter("foo,bar", services)
	assert.Empty(t, result)
}

func TestResolveServiceFilter_EmptyEntries(t *testing.T) {
	services := map[string]service.ComposeService{
		"web": {Image: "nginx"},
	}

	result := resolveServiceFilter(",web,,", services)
	assert.Len(t, result, 1)
	assert.Equal(t, []string{"web"}, result)
}

// ---------------------------------------------------------------------------
// safeName
// ---------------------------------------------------------------------------

func TestSafeName(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{
			name:  "strips leading slash",
			names: []string{"/web_0"},
			want:  "web_0",
		},
		{
			name:  "empty slice",
			names: []string{},
			want:  "",
		},
		{
			name:  "no leading slash",
			names: []string{"web"},
			want:  "web",
		},
		{
			name:  "multiple names returns first",
			names: []string{"/first", "/second"},
			want:  "first",
		},
		{
			name:  "slash only",
			names: []string{"/"},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeName(tt.names)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// readComposeFile
// ---------------------------------------------------------------------------

// helper to create a Deployer without a real Docker client (nil is fine for
// readComposeFile since it never touches the Docker socket).
func newTestDeployer() *Deployer {
	return &Deployer{}
}

func TestReadComposeFile_ValidCompose(t *testing.T) {
	dir := t.TempDir()
	content := `version: "3"
services:
  web:
    image: nginx:latest
    ports:
      - "80:80"
  api:
    image: myapi:v1
    depends_on:
      - web
`
	err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(content), 0644)
	assert.NoError(t, err)

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.NoError(t, err)
	assert.NotNil(t, cf)
	assert.Len(t, cf.Services, 2)
	assert.Equal(t, "nginx:latest", cf.Services["web"].Image)
	assert.Equal(t, "myapi:v1", cf.Services["api"].Image)
	require.Len(t, cf.Services["api"].DependsOn, 1)
	assert.Equal(t, service.DependencyConditionStarted, cf.Services["api"].DependsOn["web"].Condition)
}

func TestReadComposeFile_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	content := `version: "3"
services:
  web:
    image: nginx:latest
`
	err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(content), 0644)
	assert.NoError(t, err)

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "")
	assert.NoError(t, err)
	assert.NotNil(t, cf)
	assert.Len(t, cf.Services, 1)
}

func TestReadComposeFile_DirectoryTraversal(t *testing.T) {
	dir := t.TempDir()

	d := newTestDeployer()
	_, err := d.readComposeFile(dir, "../../etc/passwd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "directory traversal blocked")
}

func TestReadComposeFile_MissingFile(t *testing.T) {
	dir := t.TempDir()

	d := newTestDeployer()
	_, err := d.readComposeFile(dir, "nonexistent.yaml")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read compose file")
}

func TestReadComposeFile_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	content := `{{{not valid yaml at all:::}`
	err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(content), 0644)
	assert.NoError(t, err)

	d := newTestDeployer()
	_, err = d.readComposeFile(dir, "bad.yaml")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse compose YAML")
}

func TestReadComposeFile_EmptyServices(t *testing.T) {
	dir := t.TempDir()
	content := `version: "3"
services:
`
	err := os.WriteFile(filepath.Join(dir, "empty.yaml"), []byte(content), 0644)
	assert.NoError(t, err)

	d := newTestDeployer()
	_, err = d.readComposeFile(dir, "empty.yaml")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "compose file contains no services")
}

func TestReadComposeFile_SubdirectoryPath(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "deploy")
	err := os.MkdirAll(subdir, 0755)
	assert.NoError(t, err)

	content := `version: "3"
services:
  web:
    image: nginx:latest
`
	err = os.WriteFile(filepath.Join(subdir, "compose.yaml"), []byte(content), 0644)
	assert.NoError(t, err)

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "deploy/compose.yaml")
	assert.NoError(t, err)
	assert.NotNil(t, cf)
	assert.Len(t, cf.Services, 1)
}

// ---------------------------------------------------------------------------
// readComposeFile + .env interpolation
// ---------------------------------------------------------------------------

func TestReadComposeFile_DotEnvInterpolation(t *testing.T) {
	dir := t.TempDir()

	// Use docker-compose style ${VAR}, ${VAR:-default}, $VAR, and :? required.
	compose := `version: "3"
services:
  web:
    image: ${REGISTRY:-ghcr.io}/${APP}:${TAG}
    environment:
      - DEBUG=$DEBUG_FLAG
      - DB=${DB_URL:?DB_URL is required}
`
	env := `
# .env sitting next to the compose file
APP=acme-web
TAG=1.2.3
DB_URL=postgres://db/app
DEBUG_FLAG=on
# REGISTRY intentionally left unset to exercise the :-default
`
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.NoError(t, err)
	assert.NotNil(t, cf)

	web, ok := cf.Services["web"]
	assert.True(t, ok, "web service should exist after interpolation")
	assert.Equal(t, "ghcr.io/acme-web:1.2.3", web.Image)
	// Inline env list uses $DEBUG_FLAG with no braces.
	assert.Contains(t, []string(web.Environment), "DEBUG=on")
	assert.Contains(t, []string(web.Environment), "DB=postgres://db/app")
}

func TestReadComposeFile_DotEnvNextToComposeBeatsRepoRoot(t *testing.T) {
	// When both exist, the .env next to the compose wins.
	dir := t.TempDir()
	subdir := filepath.Join(dir, "deploy")
	assert.NoError(t, os.MkdirAll(subdir, 0755))

	compose := `services:
  web:
    image: nginx:${TAG}
`
	assert.NoError(t, os.WriteFile(filepath.Join(subdir, "docker-compose.yaml"), []byte(compose), 0644))
	// Root .env: would resolve to 1.0.0
	assert.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("TAG=root-wins\n"), 0644))
	// Next-to-compose .env: should win.
	assert.NoError(t, os.WriteFile(filepath.Join(subdir, ".env"), []byte("TAG=1.0.0\n"), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "deploy/docker-compose.yaml")
	assert.NoError(t, err)
	assert.Equal(t, "nginx:1.0.0", cf.Services["web"].Image)
}

func TestReadComposeFile_DotEnvRepoRootFallback(t *testing.T) {
	// No .env next to the compose — should fall back to the repo root.
	dir := t.TempDir()
	subdir := filepath.Join(dir, "deploy")
	assert.NoError(t, os.MkdirAll(subdir, 0755))

	compose := `services:
  web:
    image: nginx:${TAG}
`
	assert.NoError(t, os.WriteFile(filepath.Join(subdir, "docker-compose.yaml"), []byte(compose), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("TAG=from-root\n"), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "deploy/docker-compose.yaml")
	assert.NoError(t, err)
	assert.Equal(t, "nginx:from-root", cf.Services["web"].Image)
}

func TestReadComposeFile_NoDotEnvStillWorks(t *testing.T) {
	// Compose without any ${...} references; no .env present.
	dir := t.TempDir()
	compose := `services:
  web:
    image: nginx:1.27.0
`
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.NoError(t, err)
	assert.Equal(t, "nginx:1.27.0", cf.Services["web"].Image)
}

func TestReadComposeFile_RequiredVarMissingFails(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  web:
    image: nginx:${TAG:?TAG is required}
`
	// No .env file at all — the :? modifier should fail the read.
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))

	d := newTestDeployer()
	_, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TAG")
	assert.Contains(t, err.Error(), "required")
}

// ---------------------------------------------------------------------------
// Simple-field bundle: readComposeFile exposes the new fields on the
// parsed ComposeService struct so downstream wiring can rely on them.
// ---------------------------------------------------------------------------

func TestReadComposeFile_SimpleFieldBundle(t *testing.T) {
	dir := t.TempDir()

	compose := `services:
  web:
    image: nginx:1.27.1
    entrypoint: /docker-entrypoint.sh
    working_dir: /app
    user: "1000:1000"
    hostname: web-01
    domainname: internal.example
    expose:
      - "3000"
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
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.NoError(t, err)
	svc := cf.Services["web"]

	assert.Equal(t, service.Command{"/docker-entrypoint.sh"}, svc.Entrypoint)
	assert.Equal(t, "/app", svc.WorkingDir)
	assert.Equal(t, "1000:1000", svc.User)
	assert.Equal(t, "web-01", svc.Hostname)
	assert.Equal(t, "internal.example", svc.Domainname)
	assert.Equal(t, service.ExposeList{"3000", "9090"}, svc.Expose)
	assert.Equal(t, "30s", svc.StopGracePeriod)
	assert.Equal(t, "SIGTERM", svc.StopSignal)
	assert.Equal(t, "missing", svc.PullPolicy)
	assert.Equal(t, service.StringList{"1.1.1.1", "8.8.8.8"}, svc.DNS)
	assert.Equal(t, service.StringList{"local.example"}, svc.DNSSearch)
	assert.Equal(t, service.ExtraHosts{"host.docker.internal:10.0.0.1"}, svc.ExtraHosts)
	assert.Equal(t, []string{"SYS_PTRACE"}, svc.CapAdd)
	assert.Equal(t, []string{"MKNOD"}, svc.CapDrop)
	assert.True(t, svc.Privileged)
	assert.Equal(t, service.Tmpfs{"/run": "", "/tmp": ""}, svc.Tmpfs)
	assert.Equal(t, "256m", svc.ShmSize)
	if assert.NotNil(t, svc.Init) {
		assert.True(t, *svc.Init)
	}
}

// ---------------------------------------------------------------------------
// depends_on: long-form with conditions parses + topo-sorts correctly.
// ---------------------------------------------------------------------------

func TestReadComposeFile_DependsOnLongForm(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  app:
    image: my/app:1.0
    depends_on:
      db:
        condition: service_healthy
      migrate:
        condition: service_completed_successfully
  db:
    image: postgres:16
  migrate:
    image: my/migrate:1.0
    depends_on:
      - db
`
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.NoError(t, err)

	app := cf.Services["app"]
	require.Len(t, app.DependsOn, 2)
	assert.Equal(t, service.DependencyConditionHealthy, app.DependsOn["db"].Condition)
	assert.Equal(t, service.DependencyConditionCompletedOK, app.DependsOn["migrate"].Condition)

	// migrate uses the short form; condition should default to service_started.
	migrate := cf.Services["migrate"]
	require.Len(t, migrate.DependsOn, 1)
	assert.Equal(t, service.DependencyConditionStarted, migrate.DependsOn["db"].Condition)

	// Topological sort still produces a valid order: db before migrate,
	// both before app.
	order, err := topologicalSort([]string{"app", "db", "migrate"}, cf.Services)
	assert.NoError(t, err)
	indexOf := map[string]int{}
	for i, s := range order {
		indexOf[s] = i
	}
	assert.Less(t, indexOf["db"], indexOf["migrate"])
	assert.Less(t, indexOf["db"], indexOf["app"])
	assert.Less(t, indexOf["migrate"], indexOf["app"])
}

// ---------------------------------------------------------------------------
// stopTimeoutSeconds honours stop_grace_period with a sane fallback.
// ---------------------------------------------------------------------------

func TestStopTimeoutSeconds(t *testing.T) {
	cases := []struct {
		name string
		svc  service.ComposeService
		want int
	}{
		{"unset falls back to 10", service.ComposeService{}, 10},
		{"30 seconds", service.ComposeService{StopGracePeriod: "30s"}, 30},
		{"2 minutes", service.ComposeService{StopGracePeriod: "2m"}, 120},
		{"malformed falls back to 10", service.ComposeService{StopGracePeriod: "not-a-duration"}, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stopTimeoutSeconds(tc.svc))
		})
	}
}

// ---------------------------------------------------------------------------
// parseDNSAddrs skips invalid entries instead of failing the whole list.
// ---------------------------------------------------------------------------

func TestParseDNSAddrs(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	got := parseDNSAddrs(service.StringList{"1.1.1.1", "not-an-ip", "8.8.8.8"}, log)
	assert.Len(t, got, 2)
	assert.Equal(t, "1.1.1.1", got[0].String())
	assert.Equal(t, "8.8.8.8", got[1].String())

	assert.Nil(t, parseDNSAddrs(nil, log))
}

// ---------------------------------------------------------------------------
// Named volumes: scoping + service rewriting
// ---------------------------------------------------------------------------

func TestResolveVolumeName(t *testing.T) {
	cases := []struct {
		name    string
		stack   string
		logical string
		cfg     service.ComposeVolume
		want    string
	}{
		{
			name:    "default scopes with stack prefix",
			stack:   "prod",
			logical: "pg_data",
			cfg:     service.ComposeVolume{},
			want:    "accelero_prod_pg_data",
		},
		{
			name:    "explicit name bypasses scoping",
			stack:   "prod",
			logical: "pg_data",
			cfg:     service.ComposeVolume{Name: "shared_pg"},
			want:    "shared_pg",
		},
		{
			name:    "external uses logical name verbatim",
			stack:   "prod",
			logical: "legacy_logs",
			cfg:     service.ComposeVolume{External: true},
			want:    "legacy_logs",
		},
		{
			name:    "external + explicit name prefers explicit name",
			stack:   "prod",
			logical: "ignored",
			cfg:     service.ComposeVolume{External: true, Name: "real_name"},
			want:    "real_name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveVolumeName(tc.stack, tc.logical, tc.cfg))
		})
	}
}

func TestRewriteVolumeRefs(t *testing.T) {
	nameMap := map[string]string{
		"pg_data":     "accelero_mystack_pg_data",
		"legacy_logs": "legacy_logs", // external-style entry, maps to itself
	}

	input := []string{
		"pg_data:/var/lib/postgresql/data",            // known named volume → rewritten
		"legacy_logs:/var/log/legacy:ro",              // known + mode → rewritten
		"/host/abs:/container/abs",                    // absolute bind → untouched
		"./relative:/container/rel",                   // relative bind → untouched
		"~/home:/container/home",                      // tilde bind → untouched
		"some_unknown_volume:/container/anon",         // unknown name → leave for Docker
		"/just-a-path-no-colon",                       // malformed → untouched
	}
	got := rewriteVolumeRefs(input, nameMap)

	assert.Equal(t, []string{
		"accelero_mystack_pg_data:/var/lib/postgresql/data",
		"legacy_logs:/var/log/legacy:ro",
		"/host/abs:/container/abs",
		"./relative:/container/rel",
		"~/home:/container/home",
		"some_unknown_volume:/container/anon",
		"/just-a-path-no-colon",
	}, got)
}

func TestRewriteVolumeRefs_EmptyMapIsPassthrough(t *testing.T) {
	input := []string{"some_name:/data"}
	got := rewriteVolumeRefs(input, nil)
	assert.Equal(t, input, got)
}

// ---------------------------------------------------------------------------
// readComposeFile accepts the top-level volumes section.
// ---------------------------------------------------------------------------

func TestReadComposeFile_TopLevelVolumes(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  db:
    image: postgres:16
    volumes:
      - pg_data:/var/lib/postgresql/data
      - /host/bind:/etc/config
volumes:
  pg_data:
    driver: local
    driver_opts:
      type: tmpfs
  legacy_logs:
    external: true
    name: shared_legacy_logs
`
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	assert.NoError(t, err)
	require.Len(t, cf.Volumes, 2)

	pg := cf.Volumes["pg_data"]
	assert.Equal(t, "local", pg.Driver)
	assert.Equal(t, "tmpfs", pg.DriverOpts["type"])
	assert.False(t, pg.External)
	assert.Empty(t, pg.Name)

	legacy := cf.Volumes["legacy_logs"]
	assert.True(t, legacy.External)
	assert.Equal(t, "shared_legacy_logs", legacy.Name)
}

// suppress unused-import warnings when editing happens in bulk
var _ = service.ComposeService{}

// ---------------------------------------------------------------------------
// deploy.replicas round-trip through readComposeFile
// ---------------------------------------------------------------------------

func TestReadComposeFile_DeployReplicas(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  web:
    image: nginx:1.27.1
    deploy:
      replicas: 3
      mode: replicated
  worker:
    image: my/worker:1.0
  one-off:
    image: my/one-off:1.0
    deploy:
      replicas: 1
`
	assert.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0644))

	d := newTestDeployer()
	cf, err := d.readComposeFile(dir, "docker-compose.yaml")
	require.NoError(t, err)

	assert.Equal(t, 3, cf.Services["web"].DesiredReplicas(), "explicit replicas: 3")
	assert.Equal(t, 1, cf.Services["worker"].DesiredReplicas(), "missing deploy block defaults to 1")
	assert.Equal(t, 1, cf.Services["one-off"].DesiredReplicas(), "explicit replicas: 1")
	assert.Equal(t, "replicated", cf.Services["web"].Deploy.Mode, "mode round-trips even if unused")
}
