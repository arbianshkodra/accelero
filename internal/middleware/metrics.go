package middleware

import (
	"net/http"
	"time"

	"github.com/arbianshkodra/accelero/internal/metrics"
	"github.com/gorilla/mux"
)

// Metrics records per-request count + duration.  Path is taken from the
// mux route template where available (so /api/v1/stacks/{id} doesn't
// explode cardinality with a distinct label per stack name); falls back
// to the raw URL path when no route matched.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sr, r)

		metrics.RecordHTTPRequest(
			r.Method,
			routeTemplate(r),
			sr.status,
			time.Since(start).Seconds(),
		)
	})
}

// routeTemplate extracts the gorilla/mux template for the current route
// (e.g. "/api/v1/stacks/{id}"), falling back to the literal URL path.
func routeTemplate(r *http.Request) string {
	if route := mux.CurrentRoute(r); route != nil {
		if tpl, err := route.GetPathTemplate(); err == nil {
			return tpl
		}
	}
	return r.URL.Path
}

// statusRecorder wraps ResponseWriter so we can observe the response
// status code after the handler returns.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.written = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.written {
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}
