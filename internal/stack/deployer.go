package stack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arbianshkodra/accelero/internal/compose"
	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/metrics"
	"github.com/arbianshkodra/accelero/internal/network"
	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/arbianshkodra/accelero/internal/store"
	"github.com/arbianshkodra/accelero/internal/utils"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	dockernetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/docker/go-units"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// ComposeFile represents a parsed docker-compose.yaml.
type ComposeFile struct {
	Version  string                            `yaml:"version"`
	Services map[string]service.ComposeService `yaml:"services"`
	Networks map[string]network.ComposeNetwork `yaml:"networks,omitempty"`
	Volumes  map[string]service.ComposeVolume  `yaml:"volumes,omitempty"`
}

// serviceState captures the pre-deployment state of a single service so that
// rollback can restore it exactly as it was before the deploy started.
type serviceState struct {
	ServiceName     string
	ContainerStates []containerState
	ImageTag        string
}

// containerState captures enough information to recreate a container during rollback.
type containerState struct {
	ID         string
	Name       string
	Image      string
	Config     *container.Config
	HostConfig *container.HostConfig
	NetworkIDs []string
}

// Deployer orchestrates stack deployments against a Docker host.
type Deployer struct {
	cli   *client.Client
	store store.Store

	// stacksDir is the root under which each stack's cloned repo lives:
	//   <stacksDir>/<stack_id>/repo/
	// The clone is persisted across deploys so compose bind-mounts that
	// reference files in the repo (e.g. `./Caddyfile`) keep working.
	stacksDir string

	// Per-service mutex prevents concurrent deploys of the same service.
	serviceMu   sync.Mutex
	serviceLocks map[string]*sync.Mutex
}

// NewDeployer creates a Deployer with the given Docker client, store, and
// stable stacks-data directory. The stacks-data directory is created
// lazily during the first deploy.
func NewDeployer(cli *client.Client, s store.Store, stacksDir string) *Deployer {
	return &Deployer{
		cli:          cli,
		store:        s,
		stacksDir:    stacksDir,
		serviceLocks: make(map[string]*sync.Mutex),
	}
}

// CleanupStackData removes a stack's cloned-repo directory.  Called by the
// handler's DeleteStack after the DB record is removed so orphan clones
// don't pile up. A missing directory is not an error — stack may have had
// no deploys yet.
func (d *Deployer) CleanupStackData(stackID string) error {
	if d.stacksDir == "" {
		return nil
	}
	path := filepath.Join(d.stacksDir, stackID)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove stack data dir %q: %w", path, err)
	}
	return nil
}

// stackRepoDir returns the per-stack clone path.
func (d *Deployer) stackRepoDir(stackID string) string {
	return filepath.Join(d.stacksDir, stackID, "repo")
}

// lockService returns a per-service mutex, creating one if it does not yet exist.
func (d *Deployer) lockService(name string) *sync.Mutex {
	d.serviceMu.Lock()
	defer d.serviceMu.Unlock()
	mu, ok := d.serviceLocks[name]
	if !ok {
		mu = &sync.Mutex{}
		d.serviceLocks[name] = mu
	}
	return mu
}

// --------------------------------------------------------------------------
// Public entry point
// --------------------------------------------------------------------------

// Deploy clones the stack's git repo, parses its compose file, and deploys all
// services with zero-downtime semantics.  On failure it rolls back to the
// pre-deployment state.  It returns the Deployment record regardless of outcome.
func (d *Deployer) Deploy(ctx context.Context, stack *store.Stack, trigger string) (*store.Deployment, error) {
	deployID, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate deployment id: %w", err)
	}

	// Enrich the context so every log line from this deployment carries
	// the deployment/stack/trigger fields, including in downstream helpers.
	ctx = logctx.WithFields(ctx, logrus.Fields{
		"deployment_id": deployID,
		"stack_id":      stack.ID,
		"stack_name":    stack.Name,
		"trigger":       trigger,
	})
	log := logctx.FromContext(ctx)

	deployment := &store.Deployment{
		ID:        deployID,
		StackID:   stack.ID,
		StackName: stack.Name,
		Status:    store.DeploymentPending,
		Trigger:   trigger,
		StartedAt: time.Now(),
	}
	if err := d.store.CreateDeployment(deployment); err != nil {
		return nil, fmt.Errorf("failed to create deployment record: %w", err)
	}

	// Mark the stack as deploying.
	stack.Status = store.StackStatusDeploying
	if err := d.store.UpdateStack(stack); err != nil {
		log.WithError(err).Error("Failed to set stack status to deploying")
	}

	deployment.Status = store.DeploymentInProgress
	_ = d.store.UpdateDeployment(deployment)

	deployErr := d.executeDeploy(ctx, stack, deployment)

	now := time.Now()
	deployment.CompletedAt = &now
	stack.LastDeployedAt = &now

	if deployErr != nil {
		deployment.Status = store.DeploymentFailed
		deployment.ErrorMessage = deployErr.Error()
		stack.Status = store.StackStatusError
	} else {
		deployment.Status = store.DeploymentCompleted
		stack.Status = store.StackStatusActive
	}

	if err := d.store.UpdateDeployment(deployment); err != nil {
		log.WithError(err).Error("Failed to update deployment record")
	}
	if err := d.store.UpdateStack(stack); err != nil {
		log.WithError(err).Error("Failed to update stack record")
	}

	// Record metrics for the completed (or failed) deploy.  The deployment's
	// StartedAt is set earlier in this function so duration is always valid.
	metrics.RecordDeployment(
		stack.Name,
		trigger,
		deployment.Status,
		deployment.CompletedAt.Sub(deployment.StartedAt).Seconds(),
	)

	return deployment, deployErr
}

// --------------------------------------------------------------------------
// Core deployment pipeline
// --------------------------------------------------------------------------

func (d *Deployer) executeDeploy(ctx context.Context, stack *store.Stack, deployment *store.Deployment) error {
	log := logctx.FromContext(ctx)

	// 1. Clone repository (shallow, depth=1) into the stable per-stack dir.
	//    The directory persists after this deploy returns — compose bind
	//    mounts that reference files in the repo rely on that stable path.
	//    Cleanup happens on stack delete via CleanupStackData.
	repoDir, gitCommit, err := d.cloneRepo(ctx, stack)
	if err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	deployment.GitCommit = gitCommit
	stack.GitCommit = gitCommit
	_ = d.store.UpdateDeployment(deployment)

	// 2. Validate and read compose file.
	composeFile, err := d.readComposeFile(repoDir, stack.ComposePath)
	if err != nil {
		return fmt.Errorf("compose file error: %w", err)
	}

	// 3. Determine which services to deploy.
	servicesToDeploy := resolveServiceFilter(stack.ServiceFilter, composeFile.Services)
	if len(servicesToDeploy) == 0 {
		return fmt.Errorf("no services to deploy")
	}

	// 4. Topological sort for depends_on ordering.
	ordered, err := topologicalSort(servicesToDeploy, composeFile.Services)
	if err != nil {
		return fmt.Errorf("dependency resolution failed: %w", err)
	}
	log.Infof("Deployment order: %v", ordered)

	// 5. Capture pre-deployment state for ALL services BEFORE any deploy.
	//    This is critical for correct rollback: we snapshot the world as it
	//    exists right now, not after a partial deployment has mutated it.
	preDeployStates := make(map[string]*serviceState)
	for _, svcName := range ordered {
		state, captureErr := d.captureServiceState(ctx, svcName)
		if captureErr != nil {
			log.WithError(captureErr).Warnf("Could not capture pre-deploy state for %s", svcName)
			// Store a nil entry so we know we tried but failed.
			preDeployStates[svcName] = nil
		} else {
			preDeployStates[svcName] = state
		}
	}

	// 6. Create Docker networks.
	for netName, netConfig := range composeFile.Networks {
		if err := network.CreateNetworkWithContext(ctx, d.cli, netName, netConfig); err != nil {
			return fmt.Errorf("failed to create network %s: %w", netName, err)
		}
	}

	// 6b. Resolve & ensure named volumes.  Returns a map of logical -> actual
	//     (scoped) names so we can rewrite each service's Volumes entries.
	volumeNameMap, err := d.ensureVolumes(ctx, stack, composeFile.Volumes)
	if err != nil {
		return fmt.Errorf("volume setup failed: %w", err)
	}

	// 7. Deploy services in dependency order.
	var deployed []string
	var changes []string
	for _, svcName := range ordered {
		svc, ok := composeFile.Services[svcName]
		if !ok {
			log.Warnf("Service %s in dependency graph but missing from compose, skipping", svcName)
			continue
		}

		// Rewrite relative bind-mount source paths (e.g. `./Caddyfile`)
		// to absolute paths inside the cloned repo dir, so Docker — which
		// resolves bind-mount sources against the host — can find them.
		// Must run BEFORE rewriteVolumeRefs so the named-volume path sees
		// canonical inputs.
		rewritten, err := rewriteBindMountPaths(svc.Volumes, repoDir)
		if err != nil {
			return fmt.Errorf("service %s: %w", svcName, err)
		}
		svc.Volumes = rewriteVolumeRefs(rewritten, volumeNameMap)

		mu := d.lockService(svcName)
		mu.Lock()
		deployErr := d.deployService(ctx, stack, svcName, repoDir, svc)
		mu.Unlock()

		if deployErr != nil {
			log.WithError(deployErr).Errorf("Failed to deploy service %s, initiating rollback", svcName)

			// 8. Rollback deployed services using pre-deployment snapshots.
			d.rollbackServices(ctx, deployed, preDeployStates)
			deployment.Status = store.DeploymentRolledBack
			return fmt.Errorf("deploy of service %s failed: %w", svcName, deployErr)
		}

		deployed = append(deployed, svcName)
		changes = append(changes, fmt.Sprintf("%s -> %s", svcName, svc.Image))
	}

	deployment.Changes = strings.Join(changes, "; ")

	// 9. Update container tracking in the store.
	if err := d.syncContainerTracking(ctx, stack, ordered); err != nil {
		log.WithError(err).Warn("Failed to sync container tracking records")
	}

	log.Info("Deployment completed successfully")
	return nil
}

// --------------------------------------------------------------------------
// Git operations
// --------------------------------------------------------------------------

// cloneRepo performs a shallow clone of the stack's repository into the
// stable per-stack data dir and returns the directory, the HEAD commit
// hash, and any error.  If a previous deploy already populated this dir,
// it is wiped and re-cloned fresh — we treat the clone as an immutable
// snapshot for this deploy, never a running working tree.
func (d *Deployer) cloneRepo(ctx context.Context, stack *store.Stack) (string, string, error) {
	if stack.RepoURL == "" {
		return "", "", fmt.Errorf("repo_url is empty")
	}
	if d.stacksDir == "" {
		return "", "", fmt.Errorf("stacks data dir is not configured")
	}

	dir := d.stackRepoDir(stack.ID)
	if err := os.RemoveAll(dir); err != nil {
		return "", "", fmt.Errorf("clear previous clone at %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return "", "", fmt.Errorf("create stacks data dir: %w", err)
	}

	cloneOpts := &gogit.CloneOptions{
		URL:      stack.RepoURL,
		Depth:    1,
		Progress: nil, // do not write to stdout
		Auth: &http.BasicAuth{
			Username: stack.RepoUsername,
			Password: stack.RepoToken,
		},
	}

	if stack.RepoBranch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(stack.RepoBranch)
	}

	repo, err := gogit.PlainCloneContext(ctx, dir, false, cloneOpts)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("git clone failed: %w", err)
	}

	// Resolve HEAD commit.
	head, err := repo.Head()
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("failed to read HEAD: %w", err)
	}
	commit := head.Hash().String()

	logctx.FromContext(ctx).WithField("commit", commit[:12]).Info("Repository cloned successfully")
	return dir, commit, nil
}

// --------------------------------------------------------------------------
// Compose file handling
// --------------------------------------------------------------------------

// readComposeFile validates that composePath is safe (no directory traversal),
// reads the compose file from the cloned repo, and parses it.
func (d *Deployer) readComposeFile(repoDir, composePath string) (*ComposeFile, error) {
	if composePath == "" {
		composePath = "docker-compose.yaml"
	}

	// Security: prevent directory traversal.
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repo dir: %w", err)
	}
	target := filepath.Join(absRepo, composePath)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve compose path: %w", err)
	}
	// Ensure the resolved path is still within the repo directory.
	if !strings.HasPrefix(absTarget, absRepo+string(os.PathSeparator)) && absTarget != absRepo {
		return nil, fmt.Errorf("compose path %q resolves outside repository (directory traversal blocked)", composePath)
	}

	data, err := os.ReadFile(absTarget)
	if err != nil {
		return nil, fmt.Errorf("failed to read compose file at %s: %w", absTarget, err)
	}

	// Apply .env interpolation (${VAR}, ${VAR:-default}, etc.) before parsing.
	// We look for a .env next to the compose file first, then fall back to the
	// repo root — both are common layouts in the wild.
	vars, envPath, err := loadComposeEnv(absRepo, absTarget)
	if err != nil {
		return nil, err
	}
	if len(vars) > 0 {
		logrus.WithField("dotenv", envPath).Debugf("loaded %d variables from .env", len(vars))
	}
	expanded, err := compose.ExpandBytes(data, vars)
	if err != nil {
		return nil, fmt.Errorf("interpolate compose file: %w", err)
	}

	var cf ComposeFile
	if err := yaml.Unmarshal(expanded, &cf); err != nil {
		return nil, fmt.Errorf("failed to parse compose YAML: %w", err)
	}

	if len(cf.Services) == 0 {
		return nil, fmt.Errorf("compose file contains no services")
	}

	logrus.Infof("Parsed compose file: %d service(s), %d network(s)", len(cf.Services), len(cf.Networks))
	return &cf, nil
}

// loadComposeEnv looks for a .env file next to the compose file and then at
// the repo root, returning whichever it finds first.  The returned path is
// reported to the caller for log enrichment; both nil vars and empty path
// mean "no .env was present", which is fine.
func loadComposeEnv(repoDir, composeFilePath string) (map[string]string, string, error) {
	candidates := []string{
		filepath.Join(filepath.Dir(composeFilePath), ".env"),
		filepath.Join(repoDir, ".env"),
	}
	seen := make(map[string]bool)
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		vars, err := compose.LoadDotEnv(p)
		if err != nil {
			return nil, "", fmt.Errorf("load .env %s: %w", p, err)
		}
		if vars != nil {
			return vars, p, nil
		}
	}
	return nil, "", nil
}

// --------------------------------------------------------------------------
// Service filter & dependency resolution
// --------------------------------------------------------------------------

// resolveServiceFilter returns the list of service names that should be deployed.
// If filter is empty, all services from the compose file are included.
func resolveServiceFilter(filter string, services map[string]service.ComposeService) []string {
	if filter == "" {
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		return names
	}

	var result []string
	for _, name := range strings.Split(filter, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := services[name]; ok {
			result = append(result, name)
		} else {
			logrus.Warnf("Filtered service %q not found in compose file, skipping", name)
		}
	}
	return result
}

// topologicalSort returns the services in an order that respects depends_on,
// resolving transitive dependencies.  It detects cycles.
func topologicalSort(targets []string, allServices map[string]service.ComposeService) ([]string, error) {
	targetSet := make(map[string]bool, len(targets))
	for _, t := range targets {
		targetSet[t] = true
	}

	// Expand targets to include transitive dependencies.
	expanded := make(map[string]bool)
	var expand func(name string) error
	expand = func(name string) error {
		if expanded[name] {
			return nil
		}
		expanded[name] = true
		svc, ok := allServices[name]
		if !ok {
			return nil // missing service, will be caught later
		}
		for _, dep := range svc.DependsOn.Names() {
			if err := expand(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for _, t := range targets {
		if err := expand(t); err != nil {
			return nil, err
		}
	}

	// Kahn's algorithm for topological sort.
	inDegree := make(map[string]int)
	graph := make(map[string][]string) // dependency -> dependents

	for name := range expanded {
		if _, exists := inDegree[name]; !exists {
			inDegree[name] = 0
		}
		svc, ok := allServices[name]
		if !ok {
			continue
		}
		for _, dep := range svc.DependsOn.Names() {
			if expanded[dep] {
				graph[dep] = append(graph[dep], name)
				inDegree[name]++
			}
		}
	}

	// Seed the queue with nodes that have zero in-degree.
	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)

		for _, dependent := range graph[node] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) != len(expanded) {
		return nil, fmt.Errorf("circular dependency detected among services")
	}

	return sorted, nil
}

// --------------------------------------------------------------------------
// Pre-deployment state capture
// --------------------------------------------------------------------------

// captureServiceState snapshots the running containers for a service so they
// can be restored during rollback.
func (d *Deployer) captureServiceState(ctx context.Context, serviceName string) (*serviceState, error) {
	containers, err := d.getServiceContainers(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers for %s: %w", serviceName, err)
	}

	if len(containers) == 0 {
		return &serviceState{
			ServiceName:     serviceName,
			ContainerStates: nil,
		}, nil
	}

	var states []containerState
	var imageTag string

	for _, c := range containers {
		info, err := d.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to inspect container %s: %w", c.ID[:12], err)
		}

		var networkIDs []string
		if info.Container.NetworkSettings != nil {
			for netName := range info.Container.NetworkSettings.Networks {
				networkIDs = append(networkIDs, netName)
			}
		}

		states = append(states, containerState{
			ID:         c.ID,
			Name:       safeName(c.Names),
			Image:      c.Image,
			Config:     info.Container.Config,
			HostConfig: info.Container.HostConfig,
			NetworkIDs: networkIDs,
		})

		if imageTag == "" {
			imageTag = c.Image
		}
	}

	return &serviceState{
		ServiceName:     serviceName,
		ContainerStates: states,
		ImageTag:        imageTag,
	}, nil
}

// --------------------------------------------------------------------------
// Service deployment
// --------------------------------------------------------------------------

func (d *Deployer) deployService(ctx context.Context, stack *store.Stack, serviceName, repoDir string, svc service.ComposeService) error {
	log := logctx.FromContext(ctx).WithField("service", serviceName)

	replicas := svc.DesiredReplicas()

	// Reject replicas > 1 alongside static published host ports: N containers
	// would collide on the same host port, producing a confusing "port already
	// in use" error deep inside ContainerStart. Docker-compose's non-swarm mode
	// does the same thing. The GitOps-correct pattern is `expose:` + a proxy,
	// which is already how zero-downtime deploys route traffic here.
	if replicas > 1 && svc.HasStaticPublishedPort() {
		return fmt.Errorf("service %q has replicas=%d with a static published host port; "+
			"replicas would collide on the host port. Use `expose:` + a reverse proxy, "+
			"or reduce replicas to 1", serviceName, replicas)
	}

	// Enforce depends_on conditions before doing anything else.  This waits
	// for health or exit(0) on the declared dependencies; topological order
	// already guarantees service_started.
	if err := d.waitForDependencyConditions(ctx, serviceName, svc); err != nil {
		return err
	}

	// Resolve the image according to svc.PullPolicy.
	if err := d.resolveImage(ctx, svc, stack, log); err != nil {
		return err
	}

	// Find existing containers for this service.
	existing, err := d.getServiceContainers(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	// Partition existing containers: keep = on target image AND healthy, can
	// stay; replace = wrong image, stopped, or unhealthy, must be torn down.
	var keep []container.Summary
	var replace []container.Summary
	for _, c := range existing {
		if c.Image != svc.Image {
			replace = append(replace, c)
			continue
		}
		info, inspectErr := d.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if inspectErr != nil {
			return fmt.Errorf("failed to inspect container %s: %w", c.ID[:12], inspectErr)
		}
		state := info.Container.State
		switch {
		case state == nil, !state.Running:
			replace = append(replace, c)
		case state.Health != nil && state.Health.Status == "unhealthy":
			replace = append(replace, c)
		case state.Health != nil && state.Health.Status != "healthy":
			// Still starting — wait briefly; if it never becomes healthy
			// we'll replace it as part of the rollout.
			if err := d.waitForHealthy(ctx, c.ID); err != nil {
				log.WithError(err).Warnf("existing container %s did not become healthy, replacing", c.ID[:12])
				replace = append(replace, c)
			} else {
				keep = append(keep, c)
			}
		default:
			keep = append(keep, c)
		}
	}

	stopTimeout := stopTimeoutSeconds(svc)

	// Scale-down: too many healthy containers on the target image — trim the
	// tail into the removal list. (Extra ones are interchangeable; pick any.)
	if len(keep) > replicas {
		excess := keep[replicas:]
		keep = keep[:replicas]
		replace = append(replace, excess...)
	}

	needNew := replicas - len(keep)

	// Fast path: nothing to create, nothing to remove.
	if needNew == 0 && len(replace) == 0 {
		log.Infof("Service in sync (%d replica(s) already on target image)", replicas)
		return nil
	}

	// Rolling rollout: create one new replica, wait healthy, remove one old.
	// When `keep` is non-empty this preserves zero-downtime because at least
	// one healthy replica is always up. When `keep` is empty (fresh deploy,
	// or full replacement), there is a brief gap — same as the pre-replicas
	// behaviour with `replicas=1`.
	for i := 0; i < needNew; i++ {
		replicaIndex := len(keep) + i
		instanceName := fmt.Sprintf("%s_%d_%d", serviceName, replicaIndex, time.Now().UnixNano())
		containerID, err := d.createAndStartContainer(
			ctx, instanceName, repoDir, svc, serviceName, stack.Name, replicaIndex,
		)
		if err != nil {
			return fmt.Errorf("failed to create replica %d for %s: %w", replicaIndex, serviceName, err)
		}

		if err := d.waitForHealthy(ctx, containerID); err != nil {
			_, _ = d.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{Timeout: &stopTimeout})
			_, _ = d.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
			return fmt.Errorf("health check failed for replica %d of %s (%s): %w",
				replicaIndex, serviceName, containerID[:12], err)
		}
		log.Infof("Replica %d (%s) healthy", replicaIndex, containerID[:12])

		// Interleave: remove one old container per new one so the total
		// container count stays ~constant during the rollout.
		if len(replace) > 0 {
			oldID := replace[0].ID
			replace = replace[1:]
			d.stopAndRemove(ctx, log, oldID, stopTimeout)
		}
	}

	// Any remaining replace entries are leftovers from scale-down or from
	// cases where needNew < len(replace); tear them down now that the new
	// replicas are all healthy.
	for _, c := range replace {
		d.stopAndRemove(ctx, log, c.ID, stopTimeout)
	}

	return nil
}

// stopAndRemove is the best-effort teardown used throughout the rollout.
// Failures are logged but not fatal — a zombie container will be cleaned up
// by the next deploy or by scoped resource cleanup.
func (d *Deployer) stopAndRemove(ctx context.Context, log *logrus.Entry, id string, stopTimeout int) {
	log.Infof("Removing container %s", id[:12])
	if _, err := d.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &stopTimeout}); err != nil {
		log.WithError(err).Warnf("Failed to stop container %s", id[:12])
	}
	if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
		log.WithError(err).Warnf("Failed to remove container %s", id[:12])
	}
}

// --------------------------------------------------------------------------
// Image pulling
// --------------------------------------------------------------------------

// resolveImage applies the compose pull_policy to decide whether to pull
// the image, reuse a local copy, or fail.  Supported policies:
//
//	""                — default, same as "always"
//	"always"          — pull every deploy
//	"missing" / "if_not_present" — pull only when the image isn't local
//	"never"           — never pull; fail if the image isn't local
//	"build"           — rejected: Accelero doesn't build images from Dockerfiles
func (d *Deployer) resolveImage(ctx context.Context, svc service.ComposeService, stack *store.Stack, log *logrus.Entry) error {
	policy := strings.ToLower(strings.TrimSpace(svc.PullPolicy))

	switch policy {
	case "", "always":
		if err := d.pullImage(ctx, svc.Image, stack.DockerUsername, stack.DockerPassword, stack.DockerRegistry); err != nil {
			return fmt.Errorf("image pull failed: %w", err)
		}
		return nil

	case "missing", "if_not_present":
		present, err := d.imageIsLocal(ctx, svc.Image)
		if err != nil {
			return fmt.Errorf("image lookup failed: %w", err)
		}
		if present {
			log.WithField("image", svc.Image).Info("pull_policy=missing and image is local — skipping pull")
			return nil
		}
		if err := d.pullImage(ctx, svc.Image, stack.DockerUsername, stack.DockerPassword, stack.DockerRegistry); err != nil {
			return fmt.Errorf("image pull failed: %w", err)
		}
		return nil

	case "never":
		present, err := d.imageIsLocal(ctx, svc.Image)
		if err != nil {
			return fmt.Errorf("image lookup failed: %w", err)
		}
		if !present {
			return fmt.Errorf("pull_policy=never but image %q is not present on the host", svc.Image)
		}
		log.WithField("image", svc.Image).Info("pull_policy=never — using local image")
		return nil

	case "build":
		return fmt.Errorf("pull_policy=build is not supported: Accelero does not build images from Dockerfiles, use a CI pipeline instead")

	default:
		return fmt.Errorf("unknown pull_policy %q (expected always, missing, if_not_present, never, or build)", svc.PullPolicy)
	}
}

// imageIsLocal returns true when the Docker daemon already has the given
// image reference cached locally.
func (d *Deployer) imageIsLocal(ctx context.Context, ref string) (bool, error) {
	_, err := d.cli.ImageInspect(ctx, ref)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// pullImage pulls a Docker image, optionally authenticating with the supplied
// per-stack registry credentials (not environment variables).
func (d *Deployer) pullImage(ctx context.Context, imageRef, username, password, serverAddress string) error {
	opts := client.ImagePullOptions{}

	if username != "" && password != "" {
		authConfig := registry.AuthConfig{
			Username:      username,
			Password:      password,
			ServerAddress: serverAddress,
		}
		encoded, err := json.Marshal(authConfig)
		if err != nil {
			return fmt.Errorf("failed to encode registry auth: %w", err)
		}
		opts.RegistryAuth = base64.URLEncoding.EncodeToString(encoded)
	}

	resp, err := d.cli.ImagePull(ctx, imageRef, opts)
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", imageRef, err)
	}
	defer resp.Close()

	// Drain the stream to guarantee the pull completes before we return.
	if _, err := io.Copy(io.Discard, resp); err != nil {
		return fmt.Errorf("error reading image pull response for %s: %w", imageRef, err)
	}

	logctx.FromContext(ctx).WithField("image", imageRef).Info("Image pulled successfully")
	return nil
}

// --------------------------------------------------------------------------
// Container creation
// --------------------------------------------------------------------------

// createAndStartContainer creates a Docker container for the given compose
// service definition, starts it, and returns the container ID.
func (d *Deployer) createAndStartContainer(
	ctx context.Context,
	name, repoDir string,
	svc service.ComposeService,
	serviceName, stackName string,
	replicaIndex int,
) (string, error) {
	log := logctx.FromContext(ctx).WithFields(logrus.Fields{
		"container": name,
		"image":     svc.Image,
		"replica":   replicaIndex,
	})
	log.Info("Creating container")

	// Load environment variables from env_file entries and inline env.
	envVars, err := utils.LoadEnvFiles(svc.EnvFile, repoDir)
	if err != nil {
		return "", fmt.Errorf("failed to load env files: %w", err)
	}
	envVars = append(envVars, svc.Environment...)

	// Labels: merge user labels with accelero management labels.
	labels := make(map[string]string)
	for k, v := range svc.Labels {
		labels[k] = v
	}
	labels["managed-by"] = "accelero"
	labels["accelero-stack"] = stackName
	labels["accelero-service"] = serviceName
	labels["accelero-replica"] = strconv.Itoa(replicaIndex)

	containerConfig := &container.Config{
		Image:      svc.Image,
		Env:        envVars,
		Labels:     labels,
		Cmd:        []string(svc.Command),
		Entrypoint: []string(svc.Entrypoint),
		WorkingDir: svc.WorkingDir,
		User:       svc.User,
		Hostname:   svc.Hostname,
		Domainname: svc.Domainname,
		StopSignal: svc.StopSignal,
	}

	// stop_grace_period -> Config.StopTimeout (seconds, pointer).
	if svc.StopGracePeriod != "" {
		if dur := service.ParseDuration(svc.StopGracePeriod); dur > 0 {
			sec := int(dur.Seconds())
			containerConfig.StopTimeout = &sec
		}
	}

	if svc.HealthCheck.Test != nil {
		containerConfig.Healthcheck = &container.HealthConfig{
			Test:        svc.HealthCheck.Test,
			Interval:    service.ParseDuration(svc.HealthCheck.Interval),
			Timeout:     service.ParseDuration(svc.HealthCheck.Timeout),
			Retries:     svc.HealthCheck.Retries,
			StartPeriod: service.ParseDuration(svc.HealthCheck.StartPeriod),
		}
	}

	portBindings, exposedPorts := utils.MapPorts(svc.Ports)
	containerConfig.ExposedPorts = exposedPorts
	mergeExposedPorts(containerConfig.ExposedPorts, svc.Expose, log)

	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
		Binds:        svc.Volumes,
		DNS:          parseDNSAddrs(svc.DNS, log),
		DNSSearch:    []string(svc.DNSSearch),
		ExtraHosts:   []string(svc.ExtraHosts),
		CapAdd:       svc.CapAdd,
		CapDrop:      svc.CapDrop,
		Privileged:   svc.Privileged,
		Tmpfs:        map[string]string(svc.Tmpfs),
		Init:         svc.Init,
	}

	// shm_size — convert a human-readable string (e.g. "256m") to bytes.
	if svc.ShmSize != "" {
		if bytes, parseErr := units.RAMInBytes(svc.ShmSize); parseErr != nil {
			log.WithError(parseErr).Warnf("Invalid shm_size %q", svc.ShmSize)
		} else {
			hostConfig.ShmSize = bytes
		}
	}

	if svc.Restart != "" {
		hostConfig.RestartPolicy = container.RestartPolicy{
			Name: container.RestartPolicyMode(svc.Restart),
		}
	}

	// logging:driver + options -> HostConfig.LogConfig
	if svc.Logging != nil && svc.Logging.Driver != "" {
		hostConfig.LogConfig = container.LogConfig{
			Type:   svc.Logging.Driver,
			Config: svc.Logging.Options,
		}
	}

	// Resource limits.
	var resources container.Resources
	if svc.MemLimit != "" {
		mem, parseErr := units.RAMInBytes(svc.MemLimit)
		if parseErr != nil {
			log.WithError(parseErr).Warnf("Invalid mem_limit %q", svc.MemLimit)
		} else {
			resources.Memory = mem
		}
	}
	if svc.CPULimit != "" {
		cpuFloat, parseErr := strconv.ParseFloat(svc.CPULimit, 64)
		if parseErr != nil {
			log.WithError(parseErr).Warnf("Invalid cpu_limit %q", svc.CPULimit)
		} else {
			resources.NanoCPUs = int64(cpuFloat * 1e9)
		}
	}
	hostConfig.Resources = resources

	// Networking.
	networkingConfig := &dockernetwork.NetworkingConfig{
		EndpointsConfig: make(map[string]*dockernetwork.EndpointSettings),
	}
	for _, net := range svc.Networks {
		networkingConfig.EndpointsConfig[net] = &dockernetwork.EndpointSettings{
			Aliases: []string{serviceName},
		}
	}

	createCtx, createCancel := context.WithTimeout(ctx, 60*time.Second)
	defer createCancel()

	resp, err := d.cli.ContainerCreate(createCtx, client.ContainerCreateOptions{
		Config:           containerConfig,
		HostConfig:       hostConfig,
		NetworkingConfig: networkingConfig,
		Name:             name,
	})
	if err != nil {
		return "", fmt.Errorf("docker create failed for %s: %w", name, err)
	}

	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	defer startCancel()

	if _, err := d.cli.ContainerStart(startCtx, resp.ID, client.ContainerStartOptions{}); err != nil {
		// Attempt cleanup of the created-but-not-started container.
		_, _ = d.cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("docker start failed for %s: %w", name, err)
	}

	log.WithField("id", resp.ID[:12]).Info("Container started")
	return resp.ID, nil
}

// --------------------------------------------------------------------------
// Health check polling
// --------------------------------------------------------------------------

// waitForHealthy polls the container's health status without an unconditional
// sleep.  It checks immediately, then on a ticker.
func (d *Deployer) waitForHealthy(ctx context.Context, containerID string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Check immediately on the first iteration, then on each tick.
	for {
		info, err := d.cli.ContainerInspect(timeoutCtx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("failed to inspect container %s: %w", containerID[:12], err)
		}

		state := info.Container.State
		if state == nil {
			return fmt.Errorf("container %s has no state", containerID[:12])
		}

		// No health check defined -- consider it healthy.
		if state.Health == nil {
			logctx.FromContext(ctx).Debugf("No healthcheck defined for %s, treating as healthy", containerID[:12])
			return nil
		}

		switch state.Health.Status {
		case "healthy":
			return nil
		case "unhealthy":
			return fmt.Errorf("container %s reported unhealthy", containerID[:12])
		}

		// Wait for next tick or context cancellation.
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("health check timed out for container %s", containerID[:12])
		case <-ticker.C:
			// continue loop
		}
	}
}

// --------------------------------------------------------------------------
// Rollback
// --------------------------------------------------------------------------

// rollbackServices rolls back previously deployed services using the
// pre-deployment state snapshots.  Services are rolled back in reverse order.
func (d *Deployer) rollbackServices(ctx context.Context, deployed []string, states map[string]*serviceState) {
	log := logctx.FromContext(ctx)
	for i := len(deployed) - 1; i >= 0; i-- {
		svcName := deployed[i]
		state := states[svcName]
		if state == nil {
			log.Warnf("No pre-deploy state for %s, cannot rollback", svcName)
			continue
		}
		if err := d.rollbackService(ctx, state); err != nil {
			log.WithError(err).Errorf("Rollback failed for service %s", svcName)
		} else {
			log.Infof("Rolled back service %s successfully", svcName)
		}
	}
}

// rollbackService restores a single service to its pre-deployment state.
func (d *Deployer) rollbackService(ctx context.Context, state *serviceState) error {
	log := logctx.FromContext(ctx).WithField("service", state.ServiceName)

	if len(state.ContainerStates) == 0 {
		log.Warnf("Service %s had no containers before deployment, nothing to restore", state.ServiceName)
		return nil
	}

	log.Warnf("Rolling back to image %s", state.ImageTag)

	// Stop and remove current (post-failure) containers.
	current, err := d.getServiceContainers(ctx, state.ServiceName)
	if err != nil {
		return fmt.Errorf("failed to list current containers for rollback: %w", err)
	}
	for _, c := range current {
		stopTimeout := 10
		if _, err := d.cli.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &stopTimeout}); err != nil {
			log.WithError(err).Warnf("Failed to stop container %s during rollback", c.ID[:12])
		}
		if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("failed to remove container %s during rollback: %w", c.ID[:12], err)
		}
	}

	// Recreate containers from the saved pre-deployment configs.
	for i, cs := range state.ContainerStates {
		rollbackName := fmt.Sprintf("%s_rollback_%d_%d", state.ServiceName, i, time.Now().UnixNano())
		log.Infof("Recreating container %s from image %s", rollbackName, cs.Image)

		resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:     cs.Config,
			HostConfig: cs.HostConfig,
			Name:       rollbackName,
		})
		if err != nil {
			return fmt.Errorf("failed to create rollback container: %w", err)
		}

		// Reconnect to networks.
		for _, netID := range cs.NetworkIDs {
			if _, err := d.cli.NetworkConnect(ctx, netID, client.NetworkConnectOptions{Container: resp.ID}); err != nil {
				log.WithError(err).Warnf("Failed to connect rollback container to network %s", netID)
			}
		}

		if _, err := d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("failed to start rollback container %s: %w", resp.ID[:12], err)
		}

		log.Infof("Rollback container %s started", resp.ID[:12])
	}

	return nil
}

// --------------------------------------------------------------------------
// Container tracking
// --------------------------------------------------------------------------

// syncContainerTracking updates the store's container records to reflect the
// containers that are currently running for the stack's services.
func (d *Deployer) syncContainerTracking(ctx context.Context, stack *store.Stack, services []string) error {
	log := logctx.FromContext(ctx)

	// Remove old tracking records for this stack.
	if err := d.store.RemoveContainersByStack(stack.ID); err != nil {
		return fmt.Errorf("failed to clear old container records: %w", err)
	}

	for _, svcName := range services {
		containers, err := d.getServiceContainers(ctx, svcName)
		if err != nil {
			log.WithError(err).Warnf("Failed to list containers for tracking: %s", svcName)
			continue
		}

		for _, c := range containers {
			trackID, err := generateID()
			if err != nil {
				log.WithError(err).Warn("Failed to generate tracking ID")
				continue
			}
			mc := &store.ManagedContainer{
				ID:            trackID,
				StackID:       stack.ID,
				ServiceName:   svcName,
				ContainerID:   c.ID,
				ContainerName: safeName(c.Names),
				Image:         c.Image,
				Status:        string(c.State),
				CreatedAt:     time.Now(),
			}
			if err := d.store.TrackContainer(mc); err != nil {
				log.WithError(err).Warnf("Failed to track container %s", c.ID[:12])
			}
		}
	}

	return nil
}

// --------------------------------------------------------------------------
// Container lookup helpers
// --------------------------------------------------------------------------

// getServiceContainers lists containers whose name matches the given service,
// using Docker's name filter rather than fetching all containers.
func (d *Deployer) getServiceContainers(ctx context.Context, serviceName string) ([]container.Summary, error) {
	f := make(client.Filters).Add("name", serviceName)

	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, err
	}

	// Docker's name filter is a substring match, so we must apply strict
	// matching to avoid false positives (e.g. "web" matching "webproxy").
	var matched []container.Summary
	for _, c := range res.Items {
		for _, name := range c.Names {
			if matchesServiceName(name, serviceName) {
				matched = append(matched, c)
				break
			}
		}
	}
	return matched, nil
}

// matchesServiceName performs strict prefix matching on a container name.
// Container names from Docker typically have a leading '/'.
func matchesServiceName(containerName, serviceName string) bool {
	if len(containerName) > 0 && containerName[0] == '/' {
		containerName = containerName[1:]
	}
	return len(containerName) >= len(serviceName) &&
		containerName[:len(serviceName)] == serviceName &&
		(len(containerName) == len(serviceName) || containerName[len(serviceName)] == '_')
}

// --------------------------------------------------------------------------
// Utilities
// --------------------------------------------------------------------------

// generateID produces a cryptographically random hex ID (16 bytes = 32 hex chars).
func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// safeName extracts a clean container name from the Docker names slice.
func safeName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	name := names[0]
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	return name
}

// --------------------------------------------------------------------------
// Named volumes
// --------------------------------------------------------------------------

// resolveVolumeName produces the actual Docker-side name for a compose volume
// entry.  Rules, in priority order:
//
//  1. external: true — use the literal name (cfg.Name when set, else the
//     compose key).  Accelero neither creates nor deletes external volumes.
//  2. cfg.Name set — use it verbatim (user has explicitly taken ownership of
//     the name and accepts cross-stack collision risk).
//  3. default — scope with "accelero_<stack>_<volname>" to prevent cross-
//     stack collisions, mirroring docker-compose's project-prefix behaviour.
func resolveVolumeName(stackName, logicalName string, cfg service.ComposeVolume) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if cfg.External {
		return logicalName
	}
	return fmt.Sprintf("accelero_%s_%s", stackName, logicalName)
}

// ensureVolumes creates every internal named volume declared in the compose
// file (idempotently) and verifies that every external volume exists.
// Returns a map of the compose-level logical name -> actual Docker-side
// name so service volume references can be rewritten to match.
func (d *Deployer) ensureVolumes(ctx context.Context, stack *store.Stack, volumes map[string]service.ComposeVolume) (map[string]string, error) {
	if len(volumes) == 0 {
		return nil, nil
	}

	log := logctx.FromContext(ctx)
	out := make(map[string]string, len(volumes))

	// Sort for deterministic ordering (easier to read in logs).
	names := make([]string, 0, len(volumes))
	for name := range volumes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, logical := range names {
		cfg := volumes[logical]
		actual := resolveVolumeName(stack.Name, logical, cfg)
		out[logical] = actual

		if cfg.External {
			if err := d.verifyExternalVolume(ctx, actual); err != nil {
				return nil, fmt.Errorf("external volume %q: %w", actual, err)
			}
			log.WithField("volume", actual).Info("External volume verified")
			continue
		}

		// Idempotent: VolumeCreate with an existing name returns the existing
		// volume unchanged.
		labels := map[string]string{
			"managed-by":     "accelero",
			"accelero-stack": stack.Name,
		}
		for k, v := range cfg.Labels {
			labels[k] = v
		}

		_, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name:       actual,
			Driver:     cfg.Driver,
			DriverOpts: cfg.DriverOpts,
			Labels:     labels,
		})
		if err != nil {
			return nil, fmt.Errorf("create volume %q: %w", actual, err)
		}
		log.WithFields(logrus.Fields{"volume": actual, "driver": cfg.Driver}).Info("Volume ensured")
	}

	return out, nil
}

// verifyExternalVolume returns an error unless the named volume already
// exists on the Docker host.  External volumes are user-managed; Accelero
// refuses to silently create one that the user expected to find.
func (d *Deployer) verifyExternalVolume(ctx context.Context, name string) error {
	_, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("external volume does not exist on the host")
	}
	return err
}

// rewriteBindMountPaths walks a service's `volumes:` and rewrites every
// relative bind-mount source to an absolute path rooted at the cloned
// repo directory.  Without this pass, Docker (which resolves the source
// path on the host) would look for `./Caddyfile` relative to the
// accelero binary's working directory — almost never the right place.
//
// Rules (mirror docker-compose semantics):
//   - Absolute paths (`/etc/foo`)           → passed through unchanged
//   - Paths that look like named volumes    → passed through unchanged;
//     resolved later by rewriteVolumeRefs
//   - Relative paths (`./x`, `../y`, `x.yml`) → prefixed with repoDir
//   - Any resolved path that escapes repoDir  → deploy fails (security)
//   - `~` paths are not expanded — matches compose itself.
//
// Returning a fresh slice so callers that hold the original aren't mutated.
func rewriteBindMountPaths(svcVolumes []string, repoDir string) ([]string, error) {
	if len(svcVolumes) == 0 {
		return svcVolumes, nil
	}
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve repo dir: %w", err)
	}
	repoPrefix := absRepo + string(os.PathSeparator)

	out := make([]string, 0, len(svcVolumes))
	for _, entry := range svcVolumes {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) < 2 {
			out = append(out, entry)
			continue
		}
		src, rest := parts[0], parts[1]

		// Named-volume refs: bare identifier, no path separators.
		// Leave for rewriteVolumeRefs to handle.
		if !strings.ContainsAny(src, "/.") && !strings.HasPrefix(src, "~") {
			out = append(out, entry)
			continue
		}
		// Absolute host path: trust the user — this is the escape hatch
		// for mounting host-managed files like /etc/ssl/certs.
		if strings.HasPrefix(src, "/") {
			out = append(out, entry)
			continue
		}
		// Relative path: resolve against the cloned repo dir.
		resolved := filepath.Clean(filepath.Join(absRepo, src))
		if resolved != absRepo && !strings.HasPrefix(resolved, repoPrefix) {
			return nil, fmt.Errorf("bind mount %q resolves outside repo directory (directory traversal blocked)", src)
		}
		out = append(out, resolved+":"+rest)
	}
	return out, nil
}

// rewriteVolumeRefs returns a copy of svcVolumes with each compose-level
// logical name (the "src" part of a "src:dst[:mode]" bind) replaced by its
// resolved Docker-side name.  Bind mounts (absolute paths, `.` paths, or
// references to volumes not declared at the top level) pass through
// unchanged.
func rewriteVolumeRefs(svcVolumes []string, nameMap map[string]string) []string {
	if len(nameMap) == 0 {
		return svcVolumes
	}
	out := make([]string, 0, len(svcVolumes))
	for _, entry := range svcVolumes {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) < 2 {
			out = append(out, entry)
			continue
		}
		src := parts[0]
		// Bind mounts always have '/' or '.' — never a bare identifier.
		if strings.ContainsAny(src, "/.") || strings.HasPrefix(src, "~") {
			out = append(out, entry)
			continue
		}
		if resolved, ok := nameMap[src]; ok {
			out = append(out, resolved+":"+parts[1])
			continue
		}
		// Not a declared top-level volume — leave Docker to interpret it
		// (anonymous volume or pre-existing named volume).
		out = append(out, entry)
	}
	return out
}

// --------------------------------------------------------------------------
// Dependency conditions
// --------------------------------------------------------------------------

// dependencyWaitTimeout caps how long deployService will block waiting for a
// dependency to satisfy its condition.  Exceeds typical healthcheck
// intervals but short enough that a stuck dep surfaces as a deploy failure
// rather than hanging forever.
const dependencyWaitTimeout = 2 * time.Minute

// waitForDependencyConditions checks each entry in svc.DependsOn and blocks
// until the declared condition is satisfied.  service_started is a no-op
// because the topological sort already ensured deps deployed first.
func (d *Deployer) waitForDependencyConditions(ctx context.Context, serviceName string, svc service.ComposeService) error {
	if len(svc.DependsOn) == 0 {
		return nil
	}
	log := logctx.FromContext(ctx).WithField("service", serviceName)

	for _, depName := range svc.DependsOn.Names() {
		dep := svc.DependsOn[depName]
		switch dep.Condition {
		case "", service.DependencyConditionStarted:
			// already satisfied by deploy order
		case service.DependencyConditionHealthy:
			log.Infof("Waiting for dependency %s to be healthy", depName)
			if err := d.waitForDependencyHealthy(ctx, depName); err != nil {
				return fmt.Errorf("dependency %q must be healthy before %s: %w", depName, serviceName, err)
			}
		case service.DependencyConditionCompletedOK:
			log.Infof("Waiting for dependency %s to exit successfully", depName)
			if err := d.waitForDependencyExit(ctx, depName); err != nil {
				return fmt.Errorf("dependency %q must complete successfully before %s: %w", depName, serviceName, err)
			}
		default:
			return fmt.Errorf("dependency %q has unsupported condition %q", depName, dep.Condition)
		}
	}
	return nil
}

// waitForDependencyHealthy polls the dep's replicas until *any* of them
// reports State.Health.Status == "healthy".  "Any" mirrors Swarm's
// service_healthy semantics — Docker's embedded DNS starts resolving the
// service name to healthy endpoints as soon as one is up, so dependents
// can make forward progress without waiting for every replica.
// An absent healthcheck is treated as an error because "service_healthy"
// was explicitly requested.
func (d *Deployer) waitForDependencyHealthy(ctx context.Context, depName string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, dependencyWaitTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		containers, err := d.getServiceContainers(timeoutCtx, depName)
		if err != nil {
			return fmt.Errorf("list containers: %w", err)
		}
		if len(containers) == 0 {
			return fmt.Errorf("no containers running for dependency %s", depName)
		}

		unhealthy := 0
		for _, c := range containers {
			info, err := d.cli.ContainerInspect(timeoutCtx, c.ID, client.ContainerInspectOptions{})
			if err != nil {
				continue // transient — try again next tick
			}
			state := info.Container.State
			if state == nil {
				continue
			}
			if state.Health == nil {
				return fmt.Errorf("dependency %s has no healthcheck but service_healthy was requested", depName)
			}
			switch state.Health.Status {
			case "healthy":
				return nil
			case "unhealthy":
				unhealthy++
			}
		}
		if unhealthy == len(containers) {
			return fmt.Errorf("all %d replica(s) of dependency %s are unhealthy", len(containers), depName)
		}

		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timed out waiting for %s to become healthy", depName)
		case <-ticker.C:
		}
	}
}

// waitForDependencyExit polls until *every* replica of the dep has exited
// with status code 0.  "All" is the right semantics for one-shot / init /
// migration services: if any replica failed, the work isn't done.
func (d *Deployer) waitForDependencyExit(ctx context.Context, depName string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, dependencyWaitTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		containers, err := d.getServiceContainers(timeoutCtx, depName)
		if err != nil {
			return fmt.Errorf("list containers: %w", err)
		}
		if len(containers) == 0 {
			return fmt.Errorf("no containers found for dependency %s", depName)
		}

		allExited := true
		for _, c := range containers {
			info, err := d.cli.ContainerInspect(timeoutCtx, c.ID, client.ContainerInspectOptions{})
			if err != nil {
				allExited = false
				break
			}
			state := info.Container.State
			if state == nil {
				allExited = false
				break
			}
			if state.Running {
				allExited = false
				break
			}
			if state.ExitCode != 0 {
				return fmt.Errorf("replica %s of dependency %s exited with code %d",
					c.ID[:12], depName, state.ExitCode)
			}
		}
		if allExited {
			return nil
		}

		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timed out waiting for %s to complete", depName)
		case <-ticker.C:
		}
	}
}

// stopTimeoutSeconds returns the container stop-timeout in seconds, falling
// back to Docker's default of 10 when the compose service didn't specify
// `stop_grace_period`.
func stopTimeoutSeconds(svc service.ComposeService) int {
	if svc.StopGracePeriod == "" {
		return 10
	}
	d := service.ParseDuration(svc.StopGracePeriod)
	if d <= 0 {
		return 10
	}
	return int(d.Seconds())
}

// mergeExposedPorts adds entries from the compose `expose:` list into the
// container's ExposedPorts map.  Entries without an explicit proto default
// to "/tcp", matching docker-compose behaviour.  Invalid entries are
// warned about and skipped.
func mergeExposedPorts(exposed dockernetwork.PortSet, entries service.ExposeList, log *logrus.Entry) {
	if exposed == nil {
		return
	}
	for _, raw := range entries {
		portSpec := raw
		if !strings.Contains(portSpec, "/") {
			portSpec += "/tcp"
		}
		port, err := dockernetwork.ParsePort(portSpec)
		if err != nil {
			log.WithError(err).Warnf("Invalid expose entry %q, skipping", raw)
			continue
		}
		exposed[port] = struct{}{}
	}
}

// parseDNSAddrs converts compose DNS string entries into the typed
// []netip.Addr slice Moby expects.  Invalid addresses are warned about and
// skipped.
func parseDNSAddrs(entries service.StringList, log *logrus.Entry) []netip.Addr {
	if len(entries) == 0 {
		return nil
	}
	out := make([]netip.Addr, 0, len(entries))
	for _, raw := range entries {
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			log.WithError(err).Warnf("Invalid DNS address %q, skipping", raw)
			continue
		}
		out = append(out, addr)
	}
	return out
}
