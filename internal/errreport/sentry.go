package errreport

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// Sentry reports records to Sentry. Build it with NewSentry.
type Sentry struct {
	hub *sentry.Hub
	// diag receives Sentry's own problems -- an event the client refused,
	// transport failures -- and must NOT be wrapped by this package's
	// Handler, or a delivery failure would try to report itself.
	diag *slog.Logger
}

// NewSentry initialises a Sentry client for dsn. environment and release
// tag every event; either may be empty. diag is where delivery problems
// are logged (a plain logger on the underlying handler); without it a bad
// DSN or a revoked key reports nothing, forever, with no trace.
func NewSentry(dsn, environment, release string, diag *slog.Logger) (*Sentry, error) {
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: environment,
		Release:     release,
		// Handlers never log request headers, so nothing here needs
		// scrubbing; keep the default of not sending PII anyway.
		SendDefaultPII: false,
		// The transport's own failures (4xx from Sentry, rate limits) are
		// only ever written to its debug log; route that to diag.
		Debug:       true,
		DebugWriter: sentryDebugWriter{diag},
	})
	if err != nil {
		return nil, fmt.Errorf("sentry: %w", err)
	}
	return &Sentry{hub: sentry.NewHub(client, sentry.NewScope()), diag: diag}, nil
}

// sentryDebugWriter forwards sentry-go's debug lines to the diagnostic
// logger at Warn: the SDK only emits them for problems (dropped events,
// transport errors), not for routine sends.
type sentryDebugWriter struct{ diag *slog.Logger }

func (w sentryDebugWriter) Write(b []byte) (int, error) {
	w.diag.Warn("errreport: sentry", "detail", strings.TrimSpace(string(b)))
	return len(b), nil
}

// Report implements Reporter. Records that carry an "err" attribute are
// grouped by message plus error text; every attribute is attached under a
// "log" context.
func (s *Sentry) Report(_ context.Context, msg string, attrs map[string]any) {
	ev := sentry.NewEvent()
	ev.Level = sentry.LevelError
	ev.Message = msg
	if e, ok := attrs["err"]; ok {
		ev.Exception = []sentry.Exception{{Type: msg, Value: fmt.Sprint(e)}}
	}
	if p, ok := attrs["panic"]; ok {
		ev.Level = sentry.LevelFatal
		ev.Exception = []sentry.Exception{{Type: "panic", Value: fmt.Sprint(p)}}
	}
	ev.Contexts = map[string]sentry.Context{"log": attrs}
	if s.hub.CaptureEvent(ev) == nil {
		s.diag.Warn("errreport: sentry refused event", "msg", msg)
	}
}

// Flush blocks until queued events are sent or timeout elapses; call it on
// shutdown so the last error before an exit is not lost.
func (s *Sentry) Flush(timeout time.Duration) {
	s.hub.Flush(timeout)
}
