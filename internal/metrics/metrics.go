// Package metrics holds the Prometheus collectors Accelero exposes at
// /metrics.  All metrics are registered on a single package-level Registry
// so other packages can record data via the thin helper functions below
// without needing direct references to collectors.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the single Prometheus registry Accelero exposes.  We do not
// use the default registry so tests can create a parallel registry if they
// want to isolate metric state.
var Registry = prometheus.NewRegistry()

// ---------------------------------------------------------------------------
// Counters
// ---------------------------------------------------------------------------

var (
	deploymentsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "accelero",
			Name:      "deployments_total",
			Help:      "Total number of deployment attempts, labelled by stack, trigger, and final status.",
		},
		[]string{"stack", "trigger", "status"},
	)

	driftDetectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "accelero",
			Name:      "drift_detected_total",
			Help:      "Total drift items observed by the reconciler, labelled by stack and drift type.",
		},
		[]string{"stack", "type"},
	)

	reconcileCyclesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "accelero",
			Name:      "reconcile_cycles_total",
			Help:      "Total reconciliation ticks per stack.",
		},
		[]string{"stack"},
	)

	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "accelero",
			Name:      "http_requests_total",
			Help:      "Total HTTP requests served, labelled by method, path template, and status code class.",
		},
		[]string{"method", "path", "status"},
	)

	rateLimitedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "accelero",
			Name:      "rate_limited_requests_total",
			Help:      "HTTP requests rejected with 429 Too Many Requests by the per-API-key rate limiter, labelled by path template.",
		},
		[]string{"path"},
	)

	webhookSignatureRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "accelero",
			Name:      "webhook_signature_rejected_total",
			Help:      "Webhook requests rejected because their HMAC signature failed verification, labelled by reason (missing/malformed header, hmac mismatch).",
		},
		[]string{"reason"},
	)
)

// ---------------------------------------------------------------------------
// Histograms
// ---------------------------------------------------------------------------

var (
	deploymentDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "accelero",
			Name:      "deployment_duration_seconds",
			Help:      "Wall-clock deploy duration in seconds, labelled by stack and trigger.",
			// Buckets tuned for typical Accelero deploys: sub-second up to ~10 minutes.
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
		},
		[]string{"stack", "trigger"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "accelero",
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latency in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
)

// ---------------------------------------------------------------------------
// Gauges
// ---------------------------------------------------------------------------

var (
	stacksGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "accelero",
			Name:      "stacks",
			Help:      "Number of stacks grouped by status (active, paused, deploying, error).",
		},
		[]string{"status"},
	)

	reconcilerLoopsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "accelero",
			Name:      "reconciler_loops",
			Help:      "Number of active per-stack reconciliation loops.",
		},
	)
)

// init registers all collectors.  Process & Go runtime collectors are also
// registered so scrapers see CPU/memory/goroutine data for free.
func init() {
	Registry.MustRegister(
		deploymentsTotal,
		driftDetectedTotal,
		reconcileCyclesTotal,
		httpRequestsTotal,
		rateLimitedTotal,
		webhookSignatureRejectedTotal,
		deploymentDuration,
		httpRequestDuration,
		stacksGauge,
		reconcilerLoopsGauge,

		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{Namespace: "accelero"}),
		collectors.NewGoCollector(),
	)
}

// Handler returns the /metrics HTTP handler wired to Accelero's Registry.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// ---------------------------------------------------------------------------
// Helper functions called by the rest of the codebase
// ---------------------------------------------------------------------------

// RecordDeployment bumps the deployment counter and records the observed
// duration.  status is one of "completed", "failed", "rolled_back".
func RecordDeployment(stack, trigger, status string, durationSeconds float64) {
	deploymentsTotal.WithLabelValues(stack, trigger, status).Inc()
	deploymentDuration.WithLabelValues(stack, trigger).Observe(durationSeconds)
}

// RecordDrift increments the drift counter.  driftType values match the
// reconciler's DriftItem.Type strings ("missing", "image_mismatch", etc.).
func RecordDrift(stack, driftType string) {
	driftDetectedTotal.WithLabelValues(stack, driftType).Inc()
}

// RecordReconcileCycle bumps the reconcile-cycles counter for a stack.
func RecordReconcileCycle(stack string) {
	reconcileCyclesTotal.WithLabelValues(stack).Inc()
}

// RecordHTTPRequest records a served HTTP request.
func RecordHTTPRequest(method, path string, status int, durationSeconds float64) {
	httpRequestsTotal.WithLabelValues(method, path, statusClass(status)).Inc()
	httpRequestDuration.WithLabelValues(method, path).Observe(durationSeconds)
}

// IncRateLimited records a request rejected by the rate limiter. Path is
// the mux route template (e.g. "/api/v1/stacks/{id}") so high-cardinality
// stack IDs don't explode the label set.
func IncRateLimited(path string) {
	rateLimitedTotal.WithLabelValues(path).Inc()
}

// IncWebhookSignatureRejected records a webhook that failed HMAC
// verification. Reason is a short, closed-set tag ("missing_or_malformed_header",
// "hmac_mismatch", etc.) so the label set stays bounded.
func IncWebhookSignatureRejected(reason string) {
	webhookSignatureRejectedTotal.WithLabelValues(reason).Inc()
}

// SetStackCounts replaces the stacks gauge snapshot.  The caller should
// pass counts for every status it cares about; statuses not listed will
// keep their prior value (so callers should reset all labels each call).
func SetStackCounts(counts map[string]int) {
	stacksGauge.Reset()
	for status, n := range counts {
		stacksGauge.WithLabelValues(status).Set(float64(n))
	}
}

// SetReconcilerLoops reports the current count of active reconcile loops.
func SetReconcilerLoops(n int) {
	reconcilerLoopsGauge.Set(float64(n))
}

// statusClass maps a status code to "2xx"/"4xx"/"5xx" bucket labels —
// avoids cardinality explosion from per-status-code labels.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	case code >= 100:
		return "1xx"
	default:
		return "unknown"
	}
}
