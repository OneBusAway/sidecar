package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/export"
)

func writeExportDoc(t *testing.T, doc export.Document) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.json")
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportCommand(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 16)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	doc := export.Document{
		Format: export.Format, ExportedAt: start, RegionID: 16,
		Alerts:            []export.Alert{{ID: 99, AgencyID: "unitrans", HeaderText: "Line A rerouted", StartTime: start, Published: true}},
		PushRegistrations: []export.PushRegistration{{Token: "t1", OperatingSystem: "ios", LastSeenAt: start}},
	}
	path := writeExportDoc(t, doc)

	stdout, _, err := cli(t, dbPath, "import", "--file", path, "--dry-run")
	if err != nil || !strings.Contains(stdout, "dry run") || !strings.Contains(stdout, "1 alerts") {
		t.Fatalf("dry run: %v %q", err, stdout)
	}
	if _, getErr := store.Alerts().Get(context.Background(), 99); getErr == nil {
		t.Fatal("dry run wrote the alert")
	}

	stdout, _, err = cli(t, dbPath, "import", "--file", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "alerts             1 added, 0 already present") {
		t.Fatalf("stdout %q", stdout)
	}
	if _, getErr := store.Alerts().Get(context.Background(), 99); getErr != nil {
		t.Fatalf("alert not imported: %v", getErr)
	}

	stdout, _, err = cli(t, dbPath, "import", "--file", path)
	if err != nil || !strings.Contains(stdout, "alerts             0 added, 1 already present") {
		t.Fatalf("re-import: %v %q", err, stdout)
	}

	cliErrContains(t, dbPath, "requires --file", "import")
	two := writeExportDoc(t, doc)
	b, _ := os.ReadFile(two)
	if err := os.WriteFile(two, append(b, []byte("\n{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	cliErrContains(t, dbPath, "after the document", "import", "--file", two)
	bad := doc
	bad.RegionID = 999
	cliErrContains(t, dbPath, "region not found", "import", "--file", writeExportDoc(t, bad))
	if err := os.WriteFile(path, []byte(`{"format":"sidecar-export/1","region_id":16,"bogus":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cliErrContains(t, dbPath, "bogus", "import", "--file", path)
}
