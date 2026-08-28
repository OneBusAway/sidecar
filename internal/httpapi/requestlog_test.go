package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

func TestRequestLog_LineAndRedaction(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := sqlitetest.Open(t)
	h := httpapi.NewRouter(httpapi.Deps{
		Alerts:   store.Alerts(),
		Regions:  store.Regions(),
		Alarms:   store.Alarms(),
		PushRegs: store.PushRegs(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
		"/api/v2/regions/999/alarms/supersecret-alarm-token?user_id=secret-rider", nil)
	req.Header.Set("Authorization", "Bearer obask_1_secret")
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := buf.String()
	for _, want := range []string{"httpapi: request", "method=DELETE", `route="DELETE /api/v2/regions/{regionId}/alarms/{alarmToken}"`, "status=404", "ip=203.0.113.9", "ms="} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
	for _, leak := range []string{"supersecret-alarm-token", "secret-rider", "obask_1_secret", "user_id", "/999/"} {
		if strings.Contains(line, leak) {
			t.Errorf("log %q leaks %q", line, leak)
		}
	}
	buf.Reset()
	hz := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	h.ServeHTTP(httptest.NewRecorder(), hz)
	if buf.Len() != 0 {
		t.Errorf("/healthz logged at default level: %q", buf.String())
	}
}

func TestRequestLog_PanicBecomes500(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := sqlitetest.Open(t)
	// A panicking store stands in for any handler bug: the Regions repository
	// is hit by every region-scoped route.
	h := httpapi.NewRouter(httpapi.Deps{
		Alerts:  store.Alerts(),
		Regions: panicRegions{},
		Now:     func() time.Time { return base },
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/alerts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "httpapi: panic") || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("panic not logged: %q", buf.String())
	}
}

// panicRegions is a regions.Repository whose every method panics.
type panicRegions struct{}

func (panicRegions) Get(context.Context, int64) (regions.Region, error) { panic("boom") }
func (panicRegions) List(context.Context) ([]regions.Region, error)     { panic("boom") }
func (panicRegions) UpsertFromDirectory(context.Context, []regions.Region, time.Time) error {
	panic("boom")
}
func (panicRegions) SetLocalFields(context.Context, int64, regions.LocalFields, time.Time) error {
	panic("boom")
}
