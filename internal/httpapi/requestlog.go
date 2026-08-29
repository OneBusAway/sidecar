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

// Unwrap lets http.ResponseController reach the underlying writer's
// optional interfaces (Flush, deadlines) through the wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// requestLog writes one line per request and turns a handler panic into a
// logged 500 instead of a dropped connection. It logs the matched route
// pattern (r.Pattern, which ServeMux sets on the request before dispatch),
// never the raw path, query string, or any header: alarm and Live Activity
// tokens travel in the path, rider identifiers in the query, and long-lived
// keys in Authorization (README, proxy requirements). Lines are Info
// regardless of status -- handlers already log their own failures at the
// level they deserve, and a 5xx caused by a client hanging up is not an
// error -- except the health check at Debug, so a load balancer's probes do
// not drown everything else, and panics at Error with a stack.
func requestLog(deps Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		// Every production Deps sets Now (the route groups panic without
		// it); feed-only tests may omit it and then get zero durations.
		now := func() time.Time {
			if deps.Now == nil {
				return time.Time{}
			}
			return deps.Now()
		}
		started := now()
		defer func() {
			p := recover()
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				// A handler's deliberate abort; net/http handles it.
				panic(p)
			}
			status := rec.status
			switch {
			case p != nil && status == 0:
				// Nothing sent yet: the client can still get a real 500.
				rec.WriteHeader(http.StatusInternalServerError)
				status = http.StatusInternalServerError
			case p != nil:
				// Headers (and likely part of the body) already went out.
				// Returning normally would let net/http finish the response
				// cleanly and the client would keep a truncated body that
				// looks complete; abort so the connection is closed instead.
				defer panic(http.ErrAbortHandler)
			case status == 0:
				status = http.StatusOK // handler returned without writing; net/http sends 200
			}
			route := r.Pattern
			if route == "" {
				route = "(unmatched)"
			}
			attrs := []any{
				"method", r.Method, "route", route, "status", status, "bytes", rec.bytes,
				"ip", deps.clientIP(r), "ms", now().Sub(started).Milliseconds(),
			}
			switch {
			case p != nil:
				deps.Logger.Error("httpapi: panic", append(attrs, "panic", p, "stack", string(debug.Stack()))...)
			case route == "GET /healthz":
				deps.Logger.Debug("httpapi: request", attrs...)
			default:
				deps.Logger.Info("httpapi: request", attrs...)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}
