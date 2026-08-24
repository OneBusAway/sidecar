package httpapi

import "net/http"

// healthz is the liveness probe. It deliberately checks nothing: its job is
// to say "the process is up and routing", which is what a platform health
// check needs before a fresh deployment has any regions or users to query.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n")) //nolint:errcheck // a failed write to a probe has no one to report to
}
