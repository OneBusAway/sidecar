package adminui

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

// TestEmbedIncludesUnderscoreAndDotPaths guards the all: prefix on the embed
// directive in adminui.go. Without it the embed silently drops every "."- and
// "_"-prefixed path -- SvelteKit's entire _app/ tree -- while still
// compiling: the binary builds, the server starts, and every asset 404s.
// Nothing else in the build catches that, so assert it here.
//
// This test needs a populated dist directory: run `make web` first.
func TestEmbedIncludesUnderscoreAndDotPaths(t *testing.T) {
	fsys := FS()
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		t.Fatalf("index.html missing from the embed -- run make web: %v", err)
	}
	// The dot half of the claim. It has to be a direct Stat: the WalkDir
	// below starts at ".", whose path.Base is "." -- a HasPrefix(base, ".")
	// check inside the callback would pass unconditionally and assert
	// nothing.
	if _, err := fs.Stat(fsys, ".gitkeep"); err != nil {
		t.Errorf(".gitkeep missing from the embed: the all: prefix is gone, so dot-paths are excluded: %v", err)
	}

	var sawUnderscore bool
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(path.Base(p), "_") {
			sawUnderscore = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawUnderscore {
		t.Error("no _-prefixed path in the embed: the go:embed directive lost its all: prefix, " +
			"so SvelteKit's _app/ tree is silently excluded and every asset will 404")
	}
}
