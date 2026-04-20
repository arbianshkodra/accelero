package reconciler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arbianshkodra/accelero/internal/audit"
	"github.com/arbianshkodra/accelero/internal/compose"
	"github.com/arbianshkodra/accelero/internal/logctx"
	"github.com/arbianshkodra/accelero/internal/metrics"
	"github.com/arbianshkodra/accelero/internal/network"
	"github.com/arbianshkodra/accelero/internal/service"
	"github.com/arbianshkodra/accelero/internal/store"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// Label constants used to identify containers managed by Accelero.
const (
	labelManagedBy      = "managed-by"
	labelManagedByValue = "accelero"
	labelStackName      = "accelero-stack"
	labelServiceName    = "accelero-service"
	labelReplicaIndex   = "accelero-replica"
)

// Deployer is the interface that the stack deployer must satisfy.
type Deployer interface {
	Deploy(ctx context.Context, stack *store.Stack, trigger string) (*store.Deployment, error)
}

// DriftReport summarises the result of a single reconciliation check.
type DriftReport struct {
	StackID   string     `json:"stack_id"`
	StackName string     `json:"stack_name"`
	CheckedAt time.Time  `json:"checked_at"`
	HasDrift  bool       `json:"has_drift"`
	Drifts    []DriftItem `json:"drifts,omitempty"`
}

// DriftItem describes one specific piece of drift for a service.
type DriftItem struct {
	ServiceName string `json:"service_name"`
	Type        string `json:"type"` // missing, image_mismatch, stopped, extra, unhealthy
	Expected    string `json:"expected,omitempty"`
	Actual      string `json:"actual,omitempty"`
	Message     string `json:"message"`
}

// composeFile mirrors the structure used by the compose package.
type composeFile struct {
	Version  string                            `yaml:"version"`
	Services map[string]service.ComposeService `yaml:"services"`
	Networks map[string]network.ComposeNetwork `yaml:"networks,omitempty"`
	Volumes  map[string]service.ComposeVolume  `yaml:"volumes,omitempty"`
}

// stackLoop holds the cancellation handle for a single per-stack goroutine.
type stackLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Reconciler periodically compares desired state (git) against actual state
// (running Docker containers) for every stack that has reconciliation enabled,
// and optionally triggers deployments to converge.
type Reconciler struct {
	store    store.Store
	docker   *client.Client
	deployer Deployer

	// audit records drift observations and auto-deploy triggers as
	// system:reconciler events. nil disables audit — useful in tests
	// and minimal setups.
	audit audit.Recorder

	mu    sync.Mutex          // guards loops
	loops map[string]*stackLoop // keyed by stack ID

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// SetAudit attaches an audit recorder. Setter rather than constructor
// param to avoid churning every New() call site.
func (r *Reconciler) SetAudit(a audit.Recorder) {
	r.audit = a
}

// recordAudit is a nil-safe wrapper around the audit recorder. Failures
// are warn-logged inside the recorder; callers don't check the return.
func (r *Reconciler) recordAudit(ctx context.Context, e store.AuditEntry) {
	if r.audit == nil {
		return
	}
	_ = r.audit.Record(ctx, e)
}

// New creates a Reconciler. Call Start to begin reconciliation loops.
func New(s store.Store, dockerClient *client.Client, deployer Deployer) *Reconciler {
	return &Reconciler{
		store:    s,
		docker:   dockerClient,
		deployer: deployer,
		loops:    make(map[string]*stackLoop),
	}
}

// Start reads all stacks from the store and launches a reconcile loop for each
// one that has a positive ReconcileInterval. The provided context controls the
// overall lifetime of all loops.
func (r *Reconciler) Start(ctx context.Context) {
	r.ctx, r.cancel = context.WithCancel(ctx)

	stacks, err := r.store.ListStacks()
	if err != nil {
		logrus.Errorf("reconciler: failed to list stacks on startup: %v", err)
		return
	}

	for _, stack := range stacks {
		if stack.ReconcileInterval > 0 && stack.Status == store.StackStatusActive {
			r.startStackLoop(stack)
		}
	}

	r.publishLoopGauge()
	logrus.Infof("reconciler: started with %d active reconcile loops", len(r.loops))
}

// Stop signals all running loops to terminate and waits for them to finish.
func (r *Reconciler) Stop() {
	if r.cancel != nil {
		r.cancel()
	}

	r.mu.Lock()
	for id, loop := range r.loops {
		loop.cancel()
		delete(r.loops, id)
	}
	r.mu.Unlock()

	r.publishLoopGauge()
	r.wg.Wait()
	logrus.Info("reconciler: all loops stopped")
}

// publishLoopGauge reports the current loop count to the metrics registry.
// Must be called while holding the lock, or immediately after releasing it.
func (r *Reconciler) publishLoopGauge() {
	r.mu.Lock()
	n := len(r.loops)
	r.mu.Unlock()
	metrics.SetReconcilerLoops(n)
}

// RefreshStack should be called whenever a stack is created, updated, or
// deleted. It stops any existing loop for the stack and, if the stack still
// exists and has reconciliation enabled, starts a new one.
func (r *Reconciler) RefreshStack(stackID string) {
	log := logrus.WithField("stack_id", stackID)

	r.mu.Lock()
	if existing, ok := r.loops[stackID]; ok {
		existing.cancel()
		<-existing.done
		delete(r.loops, stackID)
	}
	r.mu.Unlock()

	stack, err := r.store.GetStack(stackID)
	if err != nil {
		log.WithError(err).Error("reconciler: failed to get stack during refresh")
		return
	}

	// Stack may have been deleted.
	if stack == nil {
		log.Info("reconciler: stack removed, loop stopped")
		return
	}

	// Enrich subsequent log lines with the resolved stack_name.
	log = log.WithField("stack_name", stack.Name)

	if stack.ReconcileInterval > 0 && stack.Status == store.StackStatusActive {
		r.startStackLoop(stack)
		log.WithField("interval_seconds", stack.ReconcileInterval).
			Info("reconciler: refreshed reconcile loop")
	} else {
		log.Info("reconciler: stack does not require a reconcile loop")
	}
	r.publishLoopGauge()
}

// CheckDrift performs a one-off drift check for the given stack. This is
// exported so that API handlers can trigger an ad-hoc check.
func (r *Reconciler) CheckDrift(ctx context.Context, stack *store.Stack) (*DriftReport, error) {
	return r.checkDrift(ctx, stack)
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// startStackLoop creates a child context, registers it, and launches the
// per-stack goroutine.
func (r *Reconciler) startStackLoop(stack *store.Stack) {
	loopCtx, loopCancel := context.WithCancel(r.ctx)
	done := make(chan struct{})

	sl := &stackLoop{
		cancel: loopCancel,
		done:   done,
	}

	r.mu.Lock()
	r.loops[stack.ID] = sl
	r.mu.Unlock()

	r.wg.Add(1)
	go r.runLoop(loopCtx, stack.ID, done)
}

// runLoop is the per-stack reconciliation goroutine.
func (r *Reconciler) runLoop(ctx context.Context, stackID string, done chan struct{}) {
	defer r.wg.Done()
	defer close(done)

	// Re-read the stack so we get the freshest interval.
	stack, err := r.store.GetStack(stackID)
	if err != nil || stack == nil {
		logrus.Errorf("reconciler: loop for stack %s could not load stack: %v", stackID, err)
		return
	}

	// Bake the stack identity into the loop's context so every log line
	// from this goroutine carries stack_id / stack_name.
	ctx = logctx.WithFields(ctx, logrus.Fields{
		"component":  "reconciler",
		"stack_id":   stack.ID,
		"stack_name": stack.Name,
	})
	log := logctx.FromContext(ctx)

	interval := time.Duration(stack.ReconcileInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Infof("loop started with interval %v", interval)

	for {
		select {
		case <-ctx.Done():
			log.Info("loop stopping")
			return

		case <-ticker.C:
			r.reconcileOnce(ctx, stackID)
		}
	}
}

// reconcileOnce performs a single reconciliation cycle for the given stack.
func (r *Reconciler) reconcileOnce(ctx context.Context, stackID string) {
	reconcileID, _ := newReconcileID()

	// Attach a per-tick reconcile_id so every drift/auto-deploy log for
	// this cycle can be correlated.  This also survives into Deploy() when
	// auto-deploy is triggered.
	ctx = logctx.WithField(ctx, "reconcile_id", reconcileID)
	log := logctx.FromContext(ctx)

	// Reload the stack each tick so we pick up config changes.
	stack, err := r.store.GetStack(stackID)
	if err != nil || stack == nil {
		log.WithError(err).Errorf("could not reload stack %s", stackID)
		return
	}

	if stack.Status != store.StackStatusActive {
		log.Debug("stack is not active, skipping reconcile")
		return
	}

	metrics.RecordReconcileCycle(stack.Name)

	report, err := r.checkDrift(ctx, stack)
	if err != nil {
		log.WithError(err).Error("drift check failed")
		return
	}

	if report.HasDrift {
		log.Infof("drift detected: %d drift(s)", len(report.Drifts))
		driftTypes := make(map[string]int, len(report.Drifts))
		for _, d := range report.Drifts {
			metrics.RecordDrift(stack.Name, d.Type)
			driftTypes[d.Type]++
			log.WithFields(logrus.Fields{
				"drift_type":   d.Type,
				"service_name": d.ServiceName,
			}).Info(d.Message)
		}

		// One audit row per reconcile cycle with drift, not per item —
		// a single stack can emit many drift items at steady state
		// (stopped containers, extra containers) and we don't want to
		// spam the audit log. Counts by type are stashed in metadata.
		driftAudit := audit.FromSystem("reconciler", store.AuditOpDriftDetected)
		driftAudit.ResourceType = "stack"
		driftAudit.ResourceID = stack.ID
		driftAudit.StackID = stack.ID
		driftAudit.StackName = stack.Name
		driftAudit.Metadata = map[string]string{
			"drift_count": fmt.Sprintf("%d", len(report.Drifts)),
		}
		for t, n := range driftTypes {
			driftAudit.Metadata["drift_type_"+t] = fmt.Sprintf("%d", n)
		}
		r.recordAudit(ctx, driftAudit)

		if stack.AutoDeploy {
			log.Info("auto-deploying to resolve drift")

			autoAudit := audit.FromSystem("reconciler", store.AuditOpDriftAutoDeployed)
			autoAudit.ResourceType = "stack"
			autoAudit.ResourceID = stack.ID
			autoAudit.StackID = stack.ID
			autoAudit.StackName = stack.Name
			autoAudit.Outcome = store.AuditOutcomeInProgress
			autoAudit.Metadata = map[string]string{
				"trigger":     store.TriggerReconcile,
				"drift_count": fmt.Sprintf("%d", len(report.Drifts)),
			}
			r.recordAudit(ctx, autoAudit)

			if _, deployErr := r.deployer.Deploy(ctx, stack, store.TriggerReconcile); deployErr != nil {
				log.WithError(deployErr).Error("auto-deploy failed")
			}
		}
	} else {
		log.Debug("no drift")
	}

	// Persist the reconciliation timestamp regardless of drift.
	now := time.Now()
	stack.LastReconciledAt = &now
	if updateErr := r.store.UpdateStack(stack); updateErr != nil {
		log.WithError(updateErr).Error("failed to update LastReconciledAt")
	}
}

// newReconcileID returns a short hex ID used to tag a single reconcile cycle.
func newReconcileID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// checkDrift compares the desired state (from git) with the actual running
// containers on the Docker host and returns a DriftReport.
func (r *Reconciler) checkDrift(ctx context.Context, stack *store.Stack) (*DriftReport, error) {
	report := &DriftReport{
		StackID:   stack.ID,
		StackName: stack.Name,
		CheckedAt: time.Now(),
	}

	// ---- 1. Clone the repository (shallow) and parse the compose file ----
	desiredServices, desiredNetworks, desiredVolumes, err := r.fetchDesiredState(ctx, stack)
	if err != nil {
		return nil, fmt.Errorf("fetch desired state: %w", err)
	}

	// ---- 2. List running containers managed by Accelero for this stack ----
	actualContainers, err := r.listStackContainers(ctx, stack.Name)
	if err != nil {
		return nil, fmt.Errorf("list stack containers: %w", err)
	}

	// Build a lookup of actual containers keyed by service name.
	type containerInfo struct {
		id     string
		image  string
		state  string // running, exited, etc.
		health string // healthy, unhealthy, starting, "" (no healthcheck)
	}

	actualByService := make(map[string][]containerInfo)
	allActualServiceNames := make(map[string]bool)

	for _, c := range actualContainers {
		svcName := c.Labels[labelServiceName]
		if svcName == "" {
			// Fall back for containers created before the accelero-service
			// label existed: derive the service from the container name
			// pattern ("<service>_<replica>_<ts>" or legacy "<service>_<ts>").
			svcName = deriveServiceName(c.Names, stack.Name)
		}
		if svcName == "" {
			continue
		}
		allActualServiceNames[svcName] = true

		info := containerInfo{
			id:    c.ID,
			image: c.Image,
			state: string(c.State),
		}

		// Inspect for health status.
		inspect, inspectErr := r.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if inspectErr == nil && inspect.Container.State != nil && inspect.Container.State.Health != nil {
			info.health = string(inspect.Container.State.Health.Status)
		}

		actualByService[svcName] = append(actualByService[svcName], info)
	}

	// Determine which services to check. If a service filter is set, honour it.
	filteredServices := filterServices(desiredServices, stack.ServiceFilter)

	// ---- 3. For each desired service, detect drift ----
	for svcName, svc := range filteredServices {
		containers := actualByService[svcName]
		desired := svc.DesiredReplicas()

		// Replica count mismatch: missing if under, surplus handled below.
		// When there are zero containers, report a single "missing" per
		// service so the drift list stays compact (rather than N items).
		if len(containers) == 0 {
			report.Drifts = append(report.Drifts, DriftItem{
				ServiceName: svcName,
				Type:        "missing",
				Expected:    svc.Image,
				Message: fmt.Sprintf("service %s is defined in compose but has no running container (want %d replica(s))",
					svcName, desired),
			})
			continue
		}
		if len(containers) < desired {
			report.Drifts = append(report.Drifts, DriftItem{
				ServiceName: svcName,
				Type:        "missing",
				Expected:    fmt.Sprintf("%d replica(s)", desired),
				Actual:      fmt.Sprintf("%d replica(s)", len(containers)),
				Message: fmt.Sprintf("service %s has %d replica(s) but %d are declared",
					svcName, len(containers), desired),
			})
		}
		if len(containers) > desired {
			surplus := len(containers) - desired
			report.Drifts = append(report.Drifts, DriftItem{
				ServiceName: svcName,
				Type:        "extra",
				Expected:    fmt.Sprintf("%d replica(s)", desired),
				Actual:      fmt.Sprintf("%d replica(s)", len(containers)),
				Message: fmt.Sprintf("service %s has %d surplus replica(s) (declared %d, actual %d)",
					svcName, surplus, desired, len(containers)),
			})
		}

		for _, c := range containers {
			// Image mismatch check. Normalise both sides: Docker may store
			// the fully-qualified image reference while the compose file may
			// use the short form or vice versa.
			if !imagesMatch(svc.Image, c.image) {
				report.Drifts = append(report.Drifts, DriftItem{
					ServiceName: svcName,
					Type:        "image_mismatch",
					Expected:    svc.Image,
					Actual:      c.image,
					Message: fmt.Sprintf("container %s has image %s, expected %s",
						shortID(c.id), c.image, svc.Image),
				})
			}

			// Stopped container check.
			if c.state != "running" {
				report.Drifts = append(report.Drifts, DriftItem{
					ServiceName: svcName,
					Type:        "stopped",
					Expected:    "running",
					Actual:      c.state,
					Message: fmt.Sprintf("container %s for service %s is %s, expected running",
						shortID(c.id), svcName, c.state),
				})
			}

			// Unhealthy container check.
			if c.health == "unhealthy" {
				report.Drifts = append(report.Drifts, DriftItem{
					ServiceName: svcName,
					Type:        "unhealthy",
					Expected:    "healthy",
					Actual:      c.health,
					Message: fmt.Sprintf("container %s for service %s is unhealthy",
						shortID(c.id), svcName),
				})
			}
		}
	}

	// ---- 4. Detect extra containers not in the compose file ----
	for svcName := range allActualServiceNames {
		if _, defined := filteredServices[svcName]; !defined {
			report.Drifts = append(report.Drifts, DriftItem{
				ServiceName: svcName,
				Type:        "extra",
				Message: fmt.Sprintf("container(s) for service %s exist but service is not defined in compose",
					svcName),
			})
		}
	}

	// ---- 5. Check network drift (desired networks that don't exist) ----
	if len(desiredNetworks) > 0 {
		existingNetworks, netErr := r.docker.NetworkList(ctx, client.NetworkListOptions{})
		if netErr == nil {
			existingSet := make(map[string]bool, len(existingNetworks.Items))
			for _, n := range existingNetworks.Items {
				existingSet[n.Name] = true
			}
			for netName := range desiredNetworks {
				if !existingSet[netName] {
					report.Drifts = append(report.Drifts, DriftItem{
						ServiceName: "(network)",
						Type:        "missing",
						Expected:    netName,
						Message:     fmt.Sprintf("network %s is defined in compose but does not exist on host", netName),
					})
				}
			}
		} else {
			logctx.FromContext(ctx).WithError(netErr).Warn("could not list Docker networks for drift check")
		}
	}

	// ---- 6. Check volume drift (declared named volumes that don't exist) ----
	if len(desiredVolumes) > 0 {
		volRes, volErr := r.docker.VolumeList(ctx, client.VolumeListOptions{})
		if volErr == nil {
			existingSet := make(map[string]bool, len(volRes.Items))
			for _, v := range volRes.Items {
				existingSet[v.Name] = true
			}
			for logicalName, cfg := range desiredVolumes {
				actualName := resolveDriftVolumeName(stack.Name, logicalName, cfg)
				if !existingSet[actualName] {
					driftType := "missing"
					msg := fmt.Sprintf("volume %q is defined in compose but does not exist on host", actualName)
					if cfg.External {
						driftType = "missing_external"
						msg = fmt.Sprintf("external volume %q is declared but does not exist on host", actualName)
					}
					report.Drifts = append(report.Drifts, DriftItem{
						ServiceName: "(volume)",
						Type:        driftType,
						Expected:    actualName,
						Message:     msg,
					})
				}
			}
		} else {
			logctx.FromContext(ctx).WithError(volErr).Warn("could not list Docker volumes for drift check")
		}
	}

	report.HasDrift = len(report.Drifts) > 0
	return report, nil
}

// resolveDriftVolumeName mirrors the deployer's scoping rule so the reconciler
// compares against the same names the deployer would create.  Kept separate
// from the deployer to avoid an import cycle; the rule is intentionally
// simple so drift stays synchronous with deploy behaviour.
func resolveDriftVolumeName(stackName, logicalName string, cfg service.ComposeVolume) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if cfg.External {
		return logicalName
	}
	return fmt.Sprintf("accelero_%s_%s", stackName, logicalName)
}

// fetchDesiredState clones the stack repository (shallow, depth=1) and parses
// the compose file. It returns the desired services, networks, and volumes,
// and always cleans up the temporary directory.
func (r *Reconciler) fetchDesiredState(ctx context.Context, stack *store.Stack) (
	map[string]service.ComposeService, map[string]network.ComposeNetwork, map[string]service.ComposeVolume, error,
) {
	tmpDir, err := secureTempDir(stack.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(tmpDir); removeErr != nil {
			logctx.FromContext(ctx).WithError(removeErr).Warnf("failed to remove temp dir %s", tmpDir)
		}
	}()

	// Clone (shallow).
	cloneOpts := &gogit.CloneOptions{
		URL:   stack.RepoURL,
		Depth: 1,
		Auth: &http.BasicAuth{
			Username: stack.RepoUsername,
			Password: stack.RepoToken,
		},
	}

	if stack.RepoBranch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(stack.RepoBranch)
	}

	_, err = gogit.PlainCloneContext(ctx, tmpDir, false, cloneOpts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("git clone: %w", err)
	}

	// Read and parse compose file.
	composePath := filepath.Join(tmpDir, stack.ComposePath)
	data, err := os.ReadFile(composePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read compose file %s: %w", stack.ComposePath, err)
	}

	// Apply .env interpolation — same preprocessing the deployer uses so the
	// drift check compares actual state against the *substituted* desired state.
	envVars, _, err := loadDotEnv(tmpDir, composePath)
	if err != nil {
		return nil, nil, nil, err
	}
	expanded, err := compose.ExpandBytes(data, envVars)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("interpolate compose file: %w", err)
	}

	var cf composeFile
	if err := yaml.Unmarshal(expanded, &cf); err != nil {
		return nil, nil, nil, fmt.Errorf("parse compose file: %w", err)
	}

	return cf.Services, cf.Networks, cf.Volumes, nil
}

// loadDotEnv looks for a .env next to the compose file, then at the repo root.
// Matches the deployer's behaviour.  A missing .env is not an error.
func loadDotEnv(repoDir, composeFilePath string) (map[string]string, string, error) {
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

// listStackContainers returns all containers on the Docker host that carry the
// Accelero management labels for the given stack name.
func (r *Reconciler) listStackContainers(ctx context.Context, stackName string) ([]container.Summary, error) {
	f := make(client.Filters).
		Add("label", fmt.Sprintf("%s=%s", labelManagedBy, labelManagedByValue)).
		Add("label", fmt.Sprintf("%s=%s", labelStackName, stackName))

	res, err := r.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, fmt.Errorf("docker container list: %w", err)
	}

	return res.Items, nil
}

// filterServices applies the stack's ServiceFilter (comma-separated list of
// service names) to the full set of desired services. If the filter is empty,
// all services are returned.
func filterServices(all map[string]service.ComposeService, filter string) map[string]service.ComposeService {
	if strings.TrimSpace(filter) == "" {
		return all
	}

	names := make(map[string]bool)
	for _, n := range strings.Split(filter, ",") {
		trimmed := strings.TrimSpace(n)
		if trimmed != "" {
			names[trimmed] = true
		}
	}

	filtered := make(map[string]service.ComposeService, len(names))
	for name, svc := range all {
		if names[name] {
			filtered[name] = svc
		}
	}
	return filtered
}

// imagesMatch compares two Docker image references, accounting for the fact
// that one may include a registry prefix / default tag while the other does not.
// e.g. "nginx:latest" matches "docker.io/library/nginx:latest" and "nginx".
func imagesMatch(desired, actual string) bool {
	if desired == actual {
		return true
	}

	// Normalise: append :latest if no tag is present.
	normalise := func(img string) string {
		// If there is a digest reference, don't touch it.
		if strings.Contains(img, "@sha256:") {
			return img
		}
		parts := strings.SplitN(img, ":", 2)
		if len(parts) == 1 {
			return img + ":latest"
		}
		return img
	}

	d := normalise(desired)
	a := normalise(actual)

	if d == a {
		return true
	}

	// Strip common registry prefixes for comparison.
	strip := func(img string) string {
		img = strings.TrimPrefix(img, "docker.io/library/")
		img = strings.TrimPrefix(img, "docker.io/")
		img = strings.TrimPrefix(img, "index.docker.io/")
		return img
	}

	return strip(d) == strip(a)
}

// deriveServiceName attempts to extract a service name from Docker container
// names and the stack name. Container names created by Accelero follow the
// pattern "<service>_<instance>_<timestamp>".
func deriveServiceName(names []string, stackName string) string {
	for _, name := range names {
		// Strip leading slash that Docker prepends.
		n := strings.TrimPrefix(name, "/")

		// If the name contains an underscore, the part before the first
		// underscore is the service name.
		if idx := strings.Index(n, "_"); idx > 0 {
			return n[:idx]
		}
	}
	return ""
}

// shortID returns the first 12 characters of a container ID for logging.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// secureTempDir creates a temporary directory for the reconciler to clone
// into. The directory is placed under os.TempDir() and uses a
// cryptographically random suffix to avoid collisions.
func secureTempDir(stackID string) (string, error) {
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return "", fmt.Errorf("generate random suffix: %w", err)
	}
	suffix := hex.EncodeToString(randBytes)

	dir := filepath.Join(os.TempDir(), fmt.Sprintf("accelero-reconcile-%s-%s", stackID, suffix))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	return dir, nil
}
