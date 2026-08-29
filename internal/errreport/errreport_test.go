package errreport_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/errreport"
)

type capture struct {
	msgs  []string
	attrs []map[string]any
}

func (c *capture) Report(_ context.Context, msg string, attrs map[string]any) {
	c.msgs = append(c.msgs, msg)
	c.attrs = append(c.attrs, attrs)
}

func TestHandler_ForwardsErrorsOnly(t *testing.T) {
	var buf bytes.Buffer
	rep := &capture{}
	log := slog.New(errreport.New(slog.NewTextHandler(&buf, nil), rep))

	log.Info("quiet", "k", 1)
	log.Warn("still quiet")
	log.With("region_id", int64(7)).WithGroup("push").Error("send failed", "err", "boom")

	if got := len(rep.msgs); got != 1 {
		t.Fatalf("reported %d records, want 1", got)
	}
	if rep.msgs[0] != "send failed" {
		t.Fatalf("msg %q", rep.msgs[0])
	}
	if rep.attrs[0]["region_id"] != int64(7) || rep.attrs[0]["push.err"] != "boom" {
		t.Fatalf("attrs %v", rep.attrs[0])
	}
	// Everything still reaches the underlying handler.
	for _, want := range []string{"quiet", "still quiet", "send failed", "region_id=7"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q: %q", want, buf.String())
		}
	}
}

func TestHandler_ReportsEvenWhenNextIsQuieter(t *testing.T) {
	var buf bytes.Buffer
	rep := &capture{}
	// A handler that only prints at Error+1 must not stop the reporter.
	next := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError + 4})
	log := slog.New(errreport.New(next, rep))
	log.Error("dropped by next")
	if len(rep.msgs) != 1 || buf.Len() != 0 {
		t.Fatalf("reported=%d printed=%q", len(rep.msgs), buf.String())
	}
}
