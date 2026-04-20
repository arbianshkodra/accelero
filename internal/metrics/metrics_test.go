package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestRecordDeployment_Increments(t *testing.T) {
	before := testutil.ToFloat64(deploymentsTotal.WithLabelValues("demo", "manual", "completed"))
	RecordDeployment("demo", "manual", "completed", 2.5)
	after := testutil.ToFloat64(deploymentsTotal.WithLabelValues("demo", "manual", "completed"))

	assert.Equal(t, before+1, after, "deployment counter should increment once per RecordDeployment call")
}

func TestRecordDrift_CountsByType(t *testing.T) {
	before := testutil.ToFloat64(driftDetectedTotal.WithLabelValues("demo", "missing"))
	RecordDrift("demo", "missing")
	RecordDrift("demo", "missing")
	RecordDrift("demo", "image_mismatch")
	after := testutil.ToFloat64(driftDetectedTotal.WithLabelValues("demo", "missing"))

	assert.Equal(t, before+2, after)
}

func TestRecordHTTPRequest_BucketsByStatusClass(t *testing.T) {
	before := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", "/metrics-test", "2xx"))
	RecordHTTPRequest("GET", "/metrics-test", 200, 0.01)
	RecordHTTPRequest("GET", "/metrics-test", 204, 0.01)
	RecordHTTPRequest("GET", "/metrics-test", 500, 0.01)
	after := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", "/metrics-test", "2xx"))

	assert.Equal(t, before+2, after, "status codes 200 and 204 both map to 2xx")

	five := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", "/metrics-test", "5xx"))
	assert.GreaterOrEqual(t, five, 1.0)
}

func TestSetStackCounts_ReplacesSnapshot(t *testing.T) {
	SetStackCounts(map[string]int{"active": 3, "paused": 1})
	assert.Equal(t, 3.0, testutil.ToFloat64(stacksGauge.WithLabelValues("active")))
	assert.Equal(t, 1.0, testutil.ToFloat64(stacksGauge.WithLabelValues("paused")))

	// New snapshot must wipe labels that no longer exist.
	SetStackCounts(map[string]int{"active": 5})
	assert.Equal(t, 5.0, testutil.ToFloat64(stacksGauge.WithLabelValues("active")))
	assert.Equal(t, 0.0, testutil.ToFloat64(stacksGauge.WithLabelValues("paused")))
}

func TestSetReconcilerLoops(t *testing.T) {
	SetReconcilerLoops(7)
	assert.Equal(t, 7.0, testutil.ToFloat64(reconcilerLoopsGauge))
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		100: "1xx",
		200: "2xx",
		204: "2xx",
		301: "3xx",
		404: "4xx",
		500: "5xx",
		0:   "unknown",
	}
	for code, want := range cases {
		assert.Equal(t, want, statusClass(code), "code=%d", code)
	}
}

func TestHandler_ExposesPrometheusFormat(t *testing.T) {
	// Put something interesting in the registry.
	RecordDeployment("handler-test", "manual", "completed", 0.1)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	Handler().ServeHTTP(rr, req)

	assert.Equal(t, 200, rr.Code)
	body := rr.Body.String()

	// Prometheus exposition format — the handler should emit HELP/TYPE lines
	// and our namespaced metric names.
	assert.Contains(t, body, "# HELP accelero_deployments_total")
	assert.Contains(t, body, "# TYPE accelero_deployments_total counter")
	assert.True(t, strings.Contains(body, `accelero_deployments_total{stack="handler-test"`),
		"metric line for the deployment we just recorded should be present:\n%s", body)
}
