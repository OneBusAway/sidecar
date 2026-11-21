// Package deploy holds no Go code; its tests pin properties of the
// deployment manifests and scripts beside them (render.yaml,
// render.staging.yaml, compose.yaml, the Dockerfile, entrypoint.sh) that
// a security review depends on, so an edit cannot silently drop them.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
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

// TestGorushTokensUnmasked (SC-06): gorush defaults log.hide_token to true,
// which masks the token in the feedback webhook payload and the inline
// sync-mode logs alike, so the sidecar could never match a dead token to a
// registration. Every manifest that runs gorush turns it off.
func TestGorushTokensUnmasked(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"render.yaml", "render.staging.yaml"} {
		s := read(t, f)
		if !strings.Contains(s, "- key: GORUSH_LOG_HIDE_TOKEN\n        value: \"false\"") {
			t.Errorf("%s: gorush service does not set GORUSH_LOG_HIDE_TOKEN=\"false\"", f)
		}
	}
	if s := read(t, "compose.yaml"); !strings.Contains(s, `GORUSH_LOG_HIDE_TOKEN: "false"`) {
		t.Error(`compose.yaml: gorush does not set GORUSH_LOG_HIDE_TOKEN: "false"`)
	}
}

// digestPinned matches an image reference pinned to a content digest.
var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}\b`)

// ownImage is this repository's own image. render*.yaml deliberately pull
// its moving :main tag -- that is the deploy mechanism (image.yml) -- so it
// is the one reference exempt from digest pinning.
const ownImage = "ghcr.io/onebusaway/sidecar:"

// TestImagesPinnedByDigest (SC-19): a mutable tag lets whoever controls the
// upstream repository (or a registry compromise) change what is built or
// run here without a commit. Every base image in the Dockerfile and every
// third-party image in the manifests is pinned by digest; the tag stays
// beside it for readers and update tooling.
func TestImagesPinnedByDigest(t *testing.T) {
	t.Parallel()
	from := regexp.MustCompile(`(?m)^FROM\s+(\S+)`)
	for _, m := range from.FindAllStringSubmatch(read(t, "Dockerfile"), -1) {
		if !digestPinned.MatchString(m[1]) {
			t.Errorf("Dockerfile: FROM %s is not pinned by digest", m[1])
		}
	}
	image := regexp.MustCompile(`(?m)^\s*(?:url|image):\s*(\S+)\s*$`)
	for _, f := range []string{"render.yaml", "render.staging.yaml", "compose.yaml"} {
		for _, m := range image.FindAllStringSubmatch(read(t, f), -1) {
			ref := m[1]
			if strings.HasPrefix(ref, ownImage) || !strings.Contains(ref, ":") {
				continue
			}
			if !digestPinned.MatchString(ref) {
				t.Errorf("%s: image %s is not pinned by digest", f, ref)
			}
		}
	}
}

// TestLitestreamDownloadVerified (SC-19): the Litestream tarball fetched at
// build time is checked against a SHA-256 recorded in the Dockerfile before
// it is unpacked, for every architecture the build supports.
func TestLitestreamDownloadVerified(t *testing.T) {
	t.Parallel()
	s := read(t, "Dockerfile")
	for _, arch := range []string{"x86_64", "arm64"} {
		if !regexp.MustCompile(`(?i)LITESTREAM_SHA256_` + arch + `=[0-9a-f]{64}`).MatchString(s) {
			t.Errorf("Dockerfile: no recorded SHA-256 for the litestream %s tarball", arch)
		}
	}
	if !strings.Contains(s, "sha256sum -c") {
		t.Error("Dockerfile: the litestream download is not verified with sha256sum -c")
	}
	if strings.Contains(s, "| tar") {
		t.Error("Dockerfile: the litestream download is piped straight into tar, unverified")
	}
}
