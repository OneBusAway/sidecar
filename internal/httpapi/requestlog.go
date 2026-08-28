package httpapi

import (
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder captures what the handler wrote so the request log can
// report it. Only the status and byte count are observed; the body passes
// straight through.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps streaming handlers working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLog writes one line per request and turns a handler panic into a
// logged 500 instead of a dropped connection. It logs the matched route
// pattern, never the raw path, query string, or any header: alarm and Live
// Activity tokens travel in the path, rider identifiers in the query, and
// long-lived keys in Authorization (README, proxy requirements). Lines are
// Info regardless of status -- handlers already log their own failures at
// the level they deserve, and a 5xx caused by a client hanging up is not
// an error -- except /healthz at Debug, so a load balancer's probes do not
// drown everything else, and panics at Error with a stack.
func requestLog(deps Deps, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		var started time.Time
		if deps.Now != nil {
			started = deps.Now()
		}
		_, route := mux.Handler(r)
		if route == "" {
			route = "(unmatched)"
		}
		defer func() {
			attrs := []any{
				"method", r.Method, "route", route, "status", rec.status,
				"bytes", rec.bytes, "ip", deps.clientIP(r),
			}
			if deps.Now != nil {
				attrs = append(attrs, "ms", deps.Now().Sub(started).Milliseconds())
			}
			if p := recover(); p != nil {
				if rec.status == 0 {
					rec.WriteHeader(http.StatusInternalServerError)
				}
				attrs[5] = rec.status
				deps.Logger.Error("httpapi: panic", append(attrs, "panic", p, "stack", string(debug.Stack()))...)
				return
			}
			if r.URL.Path == "/healthz" {
				deps.Logger.Debug("httpapi: request", attrs...)
				return
			}
			deps.Logger.Info("httpapi: request", attrs...)
		}()
		mux.ServeHTTP(rec, r)
	})
}
