package errreport

import (
	"context"
	"fmt"
	"time"

	"github.com/getsentry/sentry-go"
)

// Sentry reports records to Sentry. Build it with NewSentry; the zero value
// reports nothing.
type Sentry struct {
	hub *sentry.Hub
}

// NewSentry initialises a Sentry client for dsn. environment and release
// tag every event; either may be empty.
func NewSentry(dsn, environment, release string) (*Sentry, error) {
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: environment,
		Release:     release,
		// Handlers never log request headers, so nothing here needs
		// scrubbing; keep the default of not sending PII anyway.
		SendDefaultPII: false,
	})
	if err != nil {
		return nil, fmt.Errorf("sentry: %w", err)
	}
	return &Sentry{hub: sentry.NewHub(client, sentry.NewScope())}, nil
}

// Report implements Reporter. Records that carry an "err" attribute are
// grouped by message plus error text; every attribute is attached under a
// "log" context.
func (s *Sentry) Report(_ context.Context, msg string, attrs map[string]any) {
	if s == nil || s.hub == nil {
		return
	}
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
	s.hub.CaptureEvent(ev)
}

// Flush blocks until queued events are sent or timeout elapses; call it on
// shutdown so the last error before an exit is not lost.
func (s *Sentry) Flush(timeout time.Duration) {
	if s != nil && s.hub != nil {
		s.hub.Flush(timeout)
	}
}
