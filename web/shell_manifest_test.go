package web_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/web"
)

// TestShellAssetsMatchManifest is the deterministic guard #349 asked for: a
// SHELL_ASSETS file's content has changed without a CACHE_VERSION bump five
// times in this project's history (four commits to web/static/js/tree-modal.js,
// one to sw.js's own PR #346) and nothing caught any of them before review did.
// sw.js's cache-first fetch handler (`cacheFirst`, sw.js:174-187) means an
// installed client keeps serving the old bytes indefinitely — CACHE_VERSION is
// the only thing that ever clears them — so a miss here is silent everywhere
// except a reviewer who happens to notice.
//
// The check is deliberately not a browser test: the property that matters is
// about the repository (did this commit touch a shell asset's bytes without
// also touching CACHE_VERSION), not about runtime behaviour, and
// e2e/specs/pwa.spec.js already covers that a bump actually reaches clients.
//
// shell-manifest.json is a checked-in snapshot of the digest of every
// SHELL_ASSETS path as of the CACHE_VERSION it names. Passing requires both to
// still match what is on disk: a content edit changes a digest without
// touching cache_version and fails here; a legitimate bump changes
// cache_version and (for the assets it actually touched) their digests, and
// still fails here until shell-manifest.json is regenerated to match — which
// is the point, not a bug. That is what makes the bump "a visible, reviewed
// line" (the issue's own phrase): a PR that bumps CACHE_VERSION without
// updating shell-manifest.json does not merge either, and the failure message
// below prints the replacement file's exact content so regenerating it is a
// copy-paste, not a hand computation.
func TestShellAssetsMatchManifest(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)

	sw, err := fs.ReadFile(assets, "sw.js")
	require.NoError(t, err, "web/static/sw.js must be embedded")
	source := string(sw)

	version := parseCacheVersion(t, source)
	paths := parseShellAssets(t, source)
	require.NotEmpty(t, paths, "no SHELL_ASSETS paths parsed out of sw.js")

	actual := shellManifest{CacheVersion: version, Digests: map[string]string{}}
	for _, p := range paths {
		// "/" is SHELL_ASSETS's own path for the app shell's root document
		// (sw.js's comment: http.FileServer 301-redirects "/index.html" to
		// "/", so the shell caches the canonical path) — the file it actually
		// answers with is index.html, same as http.FileServer's own directory
		// resolution.
		name := strings.TrimPrefix(p, "/")
		if name == "" {
			name = "index.html"
		}
		body, err := fs.ReadFile(assets, name)
		require.NoErrorf(t, err, "SHELL_ASSETS lists %q, which is not an embedded asset", p)
		actual.Digests[p] = digestOf(body)
	}

	raw, err := os.ReadFile("shell-manifest.json")
	require.NoError(t, err, "web/shell-manifest.json must exist — see the test failure output below "+
		"for its expected content if this is a first run")

	var want shellManifest
	require.NoError(t, json.Unmarshal(raw, &want), "web/shell-manifest.json must be valid JSON")

	if manifestsEqual(want, actual) {
		return
	}

	replacement, err := json.MarshalIndent(actual, "", "  ")
	require.NoError(t, err)

	var mismatched []string
	for p, gotDigest := range actual.Digests {
		wantDigest, known := want.Digests[p]
		if !known {
			mismatched = append(mismatched, p+" (new — not in the manifest yet)")
			continue
		}
		if wantDigest != gotDigest {
			if want.CacheVersion == version {
				mismatched = append(mismatched,
					p+": content changed but CACHE_VERSION is still "+version+" — bump it")
			} else {
				mismatched = append(mismatched, p+": digest changed for the new version "+version)
			}
		}
	}
	for p := range want.Digests {
		if _, known := actual.Digests[p]; !known {
			mismatched = append(mismatched, p+" (removed from SHELL_ASSETS — drop it from the manifest)")
		}
	}
	if want.CacheVersion != version && len(mismatched) == 0 {
		mismatched = append(mismatched,
			"cache_version: manifest says "+want.CacheVersion+", sw.js says "+version)
	}

	t.Errorf("web/shell-manifest.json is out of date:\n  %s\n\n"+
		"Replace web/shell-manifest.json with:\n%s\n",
		strings.Join(mismatched, "\n  "), replacement)
}

type shellManifest struct {
	CacheVersion string            `json:"cache_version"`
	Digests      map[string]string `json:"digests"`
}

func manifestsEqual(a, b shellManifest) bool {
	if a.CacheVersion != b.CacheVersion || len(a.Digests) != len(b.Digests) {
		return false
	}
	for p, d := range a.Digests {
		if b.Digests[p] != d {
			return false
		}
	}
	return true
}

// digestOf hashes content with CRLF normalized to LF first. web/static's .js
// and .css files carry no .gitattributes eol pin, so a Windows checkout
// (core.autocrlf=true) embeds them with \r\n while a Linux checkout (CI) embeds
// plain \n — same bytes the application ships either way, but two different
// digests for the same content if hashed raw. Normalizing here means the
// manifest this test checks in is the same on every platform that generates
// it, and a real content edit is still exactly what changes it.
func digestOf(body []byte) string {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

var cacheVersionPattern = regexp.MustCompile(`const CACHE_VERSION = "([^"]+)";`)

func parseCacheVersion(t *testing.T, source string) string {
	t.Helper()
	match := cacheVersionPattern.FindStringSubmatch(source)
	require.NotNil(t, match, "no CACHE_VERSION parsed out of sw.js")
	return match[1]
}

var shellAssetsBlockPattern = regexp.MustCompile(`(?s)const SHELL_ASSETS = \[(.*?)\n\];`)
var quotedStringPattern = regexp.MustCompile(`"([^"]+)"`)

// parseShellAssets extracts the quoted paths from SHELL_ASSETS, the same way
// nav_test.go's TestEveryPageWiresItsOwnNavigationEntry parses nav.js. Line
// comments are stripped before scanning for quoted strings — SHELL_ASSETS
// carries an explanatory comment ahead of its first entry that itself quotes a
// path ("/index.html" is deliberately absent...), and without stripping
// comments first that quoted text is parsed as if it were a real entry.
func parseShellAssets(t *testing.T, source string) []string {
	t.Helper()
	block := shellAssetsBlockPattern.FindStringSubmatch(source)
	require.NotNil(t, block, "no SHELL_ASSETS array parsed out of sw.js")

	var cleaned []string
	for _, line := range strings.Split(block[1], "\n") {
		if i := strings.Index(line, "//"); i != -1 {
			line = line[:i]
		}
		cleaned = append(cleaned, line)
	}

	var paths []string
	for _, match := range quotedStringPattern.FindAllStringSubmatch(strings.Join(cleaned, "\n"), -1) {
		paths = append(paths, match[1])
	}
	return paths
}
