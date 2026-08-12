package alerts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

// The admin SPA repeats these three lists in TypeScript
// (web/admin/src/lib/enums.ts) because the browser cannot import a Go map.
// Drift between the two copies is not symmetric:
//
//   - An EXTRA name in the TS file is loud. The operator picks it, the server
//     rejects it, and the form shows "unknown cause …; valid values: …".
//   - A MISSING name is completely silent. Someone adds a cause here, forgets
//     the TS file, and the option simply never appears in the form. No error,
//     no log line, nothing to notice -- the feature is just quietly absent.
//
// So the loud direction needs no test and the silent one cannot be caught any
// other way. This asserts both, because a one-directional check would leave
// the same class of hole in the other direction the next time someone edits
// only the TS file.
//
// Reaching into the web tree from a Go test is already precedent here:
// internal/httpapi/adminui/adminui_test.go asserts against the built SPA.

// tsEnumsPath is web/admin/src/lib/enums.ts relative to this package.
const tsEnumsPath = "../../web/admin/src/lib/enums.ts"

// quotedName matches the single-quoted enum names inside one array literal.
var quotedName = regexp.MustCompile(`'([A-Z][A-Z_]*)'`)

// tsEnumNames returns the names in `export const <constName>: EnumOption[] = [
// ... ]`.
//
// The slice is bounded by the FIRST `]` after the opening bracket, so a later
// array in the file cannot bleed into this one; the names are then matched by
// shape rather than by line, so reformatting the file does not break this.
func tsEnumNames(t *testing.T, source, constName string) []string {
	t.Helper()

	marker := "export const " + constName + ": EnumOption[] = ["
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("%s: no %q declaration found; if it was renamed, rename it here too",
			tsEnumsPath, constName)
	}
	rest := source[start+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatalf("%s: %s array is not terminated", tsEnumsPath, constName)
	}

	var names []string
	for _, m := range quotedName.FindAllStringSubmatch(rest[:end], -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatalf("%s: %s parsed as empty -- the file's shape changed and this "+
			"test would otherwise pass by matching nothing", tsEnumsPath, constName)
	}
	return names
}

// validNames pulls the authoritative list out of the parser's own error.
//
// The tables are unexported and this is an external test package, so rather
// than duplicating the names here -- a third copy, with its own drift -- this
// reads the list the server actually validates against, which is also the list
// the operator is shown when they get it wrong.
func validNames(t *testing.T, parse func(string) (string, error)) []string {
	t.Helper()

	_, err := parse("__no_such_value__")
	if err == nil {
		t.Fatal("parser accepted a nonsense value, so it cannot report the valid ones")
	}
	const sep = "valid values: "
	i := strings.Index(err.Error(), sep)
	if i < 0 {
		t.Fatalf("error %q no longer lists the valid values; this test reads them from it", err)
	}
	var names []string
	for _, n := range strings.Split(err.Error()[i+len(sep):], ",") {
		names = append(names, strings.TrimSpace(n))
	}
	return names
}

func TestSPAEnumListsMatchGo(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Clean(tsEnumsPath))
	if err != nil {
		t.Fatalf("read %s: %v", tsEnumsPath, err)
	}
	source := string(raw)

	for _, tc := range []struct {
		constName string
		parse     func(string) (string, error)
	}{
		{"CAUSES", alerts.ParseCause},
		{"EFFECTS", alerts.ParseEffect},
		{"SEVERITIES", alerts.ParseSeverity},
	} {
		t.Run(tc.constName, func(t *testing.T) {
			t.Parallel()

			inGo := validNames(t, tc.parse)
			inTS := tsEnumNames(t, source, tc.constName)

			goSet := make(map[string]bool, len(inGo))
			for _, n := range inGo {
				goSet[n] = true
			}
			tsSet := make(map[string]bool, len(inTS))
			for _, n := range inTS {
				tsSet[n] = true
			}

			var missing, extra []string
			for n := range goSet {
				if !tsSet[n] {
					missing = append(missing, n)
				}
			}
			for n := range tsSet {
				if !goSet[n] {
					extra = append(extra, n)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)

			if len(missing) > 0 {
				t.Errorf("%s: %s is missing %v -- these are valid here but the "+
					"admin form will never offer them, with no error anywhere",
					tsEnumsPath, tc.constName, missing)
			}
			if len(extra) > 0 {
				t.Errorf("%s: %s offers %v, which this package rejects -- the "+
					"operator gets a 400 on submit",
					tsEnumsPath, tc.constName, extra)
			}
			if len(inTS) != len(tsSet) {
				t.Errorf("%s: %s contains duplicate names (%d entries, %d distinct)",
					tsEnumsPath, tc.constName, len(inTS), len(tsSet))
			}
		})
	}
}
