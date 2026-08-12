// Package adminui embeds the built admin SPA. The dist directory is
// populated by `make web` and gitignored except for .gitkeep; the all:
// prefix is load-bearing twice over: plain go:embed patterns exclude files
// beginning with "." or "_", which would drop both .gitkeep and SvelteKit's
// entire _app/ output tree -- the build would succeed and every asset 404
// (design spec section 2.3).
package adminui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded SPA rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the dist directory vanishes from the module,
		// which the committed .gitkeep prevents; a broken binary should say
		// so at boot, not serve mystery 500s.
		panic("adminui: embedded dist directory missing: " + err.Error())
	}
	return sub
}
