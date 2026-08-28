// Package errreport forwards error-level log records to an external error
// tracker. It hangs off slog rather than off call sites so that every
// existing Logger.Error in a handler or background loop becomes a tracked
// event with no per-site wiring, and so that tests can observe reporting
// through a fake Reporter instead of a network.
package errreport

import (
	"context"
	"log/slog"
)

// Reporter receives one error-level record at a time. Attrs are the
// record's flattened attributes; implementations must not block for long,
// since they run on the logging call's goroutine.
type Reporter interface {
	Report(ctx context.Context, msg string, attrs map[string]any)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(ctx context.Context, msg string, attrs map[string]any)

// Report implements Reporter.
func (f ReporterFunc) Report(ctx context.Context, msg string, attrs map[string]any) {
	f(ctx, msg, attrs)
}

// Handler is a slog.Handler that passes every record to Next and, for
// records at slog.LevelError or above, also to Reporter. Attributes and
// groups added through WithAttrs/WithGroup reach both.
type Handler struct {
	next     slog.Handler
	reporter Reporter
	attrs    []slog.Attr
	group    string
}

// New wraps next so error records also reach reporter.
func New(next slog.Handler, reporter Reporter) *Handler {
	return &Handler{next: next, reporter: reporter}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelError || h.next.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		attrs := make(map[string]any, r.NumAttrs()+len(h.attrs))
		for _, a := range h.attrs {
			attrs[a.Key] = a.Value.Resolve().Any()
		}
		r.Attrs(func(a slog.Attr) bool {
			attrs[h.key(a.Key)] = a.Value.Resolve().Any()
			return true
		})
		h.reporter.Report(ctx, r.Message, attrs)
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *Handler) key(k string) string {
	if h.group == "" {
		return k
	}
	return h.group + "." + k
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	// Keys are qualified with the group open at the time they were added,
	// matching how slog's own handlers scope earlier attrs.
	c.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = append(c.attrs, slog.Attr{Key: h.key(a.Key), Value: a.Value})
	}
	return &c
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = h.key(name)
	return &c
}
