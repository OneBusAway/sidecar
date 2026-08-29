package main

import (
	"os"
	"strings"
	"testing"
)

// TestStagingBlueprintMirrorsProduction pins render.staging.yaml to
// render.yaml: the staging file must be the production one with exactly
// the substitutions below and nothing else, so a change to one Blueprint
// cannot silently miss the other (the first hand copy shipped with
// staging attached to production's env group).
func TestStagingBlueprintMirrorsProduction(t *testing.T) {
	t.Parallel()
	prod, err := os.ReadFile("../../render.yaml")
	if err != nil {
		t.Fatal(err)
	}
	staging, err := os.ReadFile("../../render.staging.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Each file's leading comment block is its own; only the YAML body is
	// mirrored.
	body := func(b []byte) string {
		s := string(b)
		i := strings.Index(s, "envVarGroups:")
		if i < 0 {
			t.Fatal("Blueprint has no envVarGroups section")
		}
		return s[i:]
	}
	want := body(prod)
	for _, sub := range [][2]string{
		{"sidecar-shared", "sidecar-staging-shared"},
		{"name: sidecar\n", "name: sidecar-staging\n"},
		{"name: gorush\n", "name: gorush-staging\n"},
		{"name: sidecar-data", "name: sidecar-staging-data"},
		{"      - key: GORUSH_IOS_PRODUCTION\n        value: \"true\"", "      - key: GORUSH_IOS_PRODUCTION\n        value: \"false\""},
		{"      - key: SIDECAR_SENTRY_ENVIRONMENT\n        value: production", "      - key: SIDECAR_SENTRY_ENVIRONMENT\n        value: staging"},
		{"      - key: SIDECAR_BACKUP_BUCKET\n        sync: false\n", "      - key: SIDECAR_BACKUP_BUCKET\n        sync: false\n      # Keep staging's replica apart from production's in a shared bucket.\n      - key: SIDECAR_BACKUP_PATH\n        value: sidecar-staging\n"},
		{"      - key: SIDECAR_DB\n        value: /data/sidecar.db\n", "      - key: SIDECAR_DB\n        value: /data/sidecar.db\n      # A hand-maintained directory whose regions all point at this host, so\n      # TestFlight builds that use the custom regions URL land here and\n      # production devices never do (README, Staging).\n      - key: SIDECAR_REGIONS_URL\n        sync: false\n"},
	} {
		if !strings.Contains(want, sub[0]) {
			t.Fatalf("render.yaml no longer contains %q; update the substitution table", sub[0])
		}
		want = strings.ReplaceAll(want, sub[0], sub[1])
	}
	if got := body(staging); got != want {
		t.Fatalf("render.staging.yaml has drifted from render.yaml; regenerate it by applying this test's substitutions to render.yaml\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
