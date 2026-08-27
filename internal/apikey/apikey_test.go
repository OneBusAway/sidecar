package apikey_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

// TestLogValueOmitsHashes is the leak test. A hash is not a live credential,
// but it IS the lookup key: an attacker holding a database backup plus a hash
// from a log line learns nothing new, while an attacker holding only the log
// learns which row to target. regions.Region.LogValue omits the OBA key for
// the same reason; these two types follow it.
func TestLogValueOmitsHashes(t *testing.T) {
	t.Parallel()

	const hash = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("region key",
		"key", apikey.RegionKey{
			ID: 3, RegionID: 1, Name: "obacloud", KeyHash: hash,
			CreatedBy: apikey.Actor{Kind: apikey.ActorPrincipal, ID: 2}, CreatedAt: at,
		})
	logger.Info("principal",
		"principal", apikey.ServicePrincipal{ID: 2, Name: "rails", KeyHash: hash, CreatedAt: at})

	if strings.Contains(buf.String(), hash) {
		t.Errorf("log output contains the key hash:\n%s", buf.String())
	}
	for _, want := range []string{"id=3", "region_id=1", "name=obacloud"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log output missing %q:\n%s", want, buf.String())
		}
	}
}
