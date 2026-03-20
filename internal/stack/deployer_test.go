package stack

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/stretchr/testify/assert"
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

func TestTopologicalSort_LinearChain(t *testing.T) {
	// A depends on B, B depends on C  =>  order should be C, B, A.
	services := map[string]service.ComposeService{
		"A": {Image: "a", DependsOn: []string{"B"}},
		"B": {Image: "b", DependsOn: []string{"C"}},
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
		"A": {Image: "a", DependsOn: []string{"B", "C"}},
		"B": {Image: "b", DependsOn: []string{"D"}},
		"C": {Image: "c", DependsOn: []string{"D"}},
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
		"A": {Image: "a", DependsOn: []string{"B"}},
		"B": {Image: "b", DependsOn: []string{"A"}},
	}
	targets := []string{"A", "B"}

	_, err := topologicalSort(targets, services)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "circular dependency")
}

func TestTopologicalSort_TransitiveDepsIncluded(t *testing.T) {
	// Target is only A, but A -> B -> C, so B and C must also be included.
	services := map[string]service.ComposeService{
		"A": {Image: "a", DependsOn: []string{"B"}},
		"B": {Image: "b", DependsOn: []string{"C"}},
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
	assert.Equal(t, []string{"web"}, cf.Services["api"].DependsOn)
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
