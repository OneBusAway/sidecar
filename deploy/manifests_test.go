// Package deploy holds no Go code; its tests pin properties of the
// deployment manifests and scripts beside them (render.yaml,
// render.staging.yaml, compose.yaml, the Dockerfile, entrypoint.sh) that
// a security review depends on, so an edit cannot silently drop them.
package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEntrypoint_OwnerOnlyUmask (SC-16): Litestream's restored database and
// its page copies are created owner-only.
func TestEntrypoint_OwnerOnlyUmask(t *testing.T) {
	t.Parallel()
	s := read(t, "deploy/entrypoint.sh")
	umask := strings.Index(s, "\numask 077\n")
	if umask < 0 {
		t.Fatal("deploy/entrypoint.sh does not set umask 077")
	}
	if restore := strings.Index(s, "litestream restore"); restore < umask {
		t.Fatal("umask 077 must come before litestream restore")
	}
}
