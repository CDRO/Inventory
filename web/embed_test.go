package web_test

import (
	"bytes"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/web"
)

// TestStaticIsRootedAtStaticDir pins the fs.Sub rooting. Getting it wrong is
// silent: the embed still succeeds, the binary still builds, and every asset
// simply moves to /static/... so the site 404s in production while every
// handler test that passes StaticFS: nil stays green.
func TestStaticIsRootedAtStaticDir(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)

	for _, name := range []string{"index.html", "css/base.css"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body, err := fs.ReadFile(assets, name)
			require.NoError(t, err, "%s must be reachable at the root of the embedded tree", name)
			assert.NotEmpty(t, body, "%s embedded as an empty file", name)
		})
	}

	_, err = fs.ReadFile(assets, "static/index.html")
	assert.Error(t, err, "the static/ prefix must be stripped, not preserved")
}

// TestTemplatesStayOutOfTheStaticTree pins the separation between what the
// browser can fetch by path and what only the server renders. An admin template
// reachable as a static asset would ship the admin area's shape to every
// visitor, which docs/specs/03-auth-and-multi-tenancy.md rules out.
func TestTemplatesStayOutOfTheStaticTree(t *testing.T) {
	t.Parallel()

	tmpl, err := web.Templates()
	require.NoError(t, err)
	body, err := fs.ReadFile(tmpl, "admin.html")
	require.NoError(t, err, "admin.html must be at the root of the templates tree")
	assert.NotEmpty(t, body)

	assets, err := web.Static()
	require.NoError(t, err)
	require.NoError(t, fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		assert.NotContains(t, path, "admin", "no admin asset may be servable from the static tree")
		return nil
	}))
}

// TestStaticEmbedsTheWholeTree guards against a //go:embed pattern that
// matches the directory but silently drops nested files.
func TestStaticEmbedsTheWholeTree(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)

	var files []string
	require.NoError(t, fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	}))

	assert.Contains(t, files, "index.html")
	assert.Contains(t, files, "css/base.css", "nested assets must survive the embed")
}

// TestNoStaticAssetNamesTheAdminArea is the path check above carried through
// to content, which is where it actually bites.
//
// The rule is docs/specs/03-auth-and-multi-tenancy.md's: the shipped
// JavaScript contains no admin code and no navigation to the admin area, and
// the area does not announce its own existence. Keeping admin.html out of
// static/ satisfies the first half; it does nothing about a comment, a string
// or a href inside a file that *is* served. An HTML comment is the sharp case,
// because it reaches the browser byte-for-byte: settings.html carried "the
// admin password reset lives on the server-rendered /admin page" until this
// test was written, which handed every visitor the path that answers 404 to
// them precisely so they cannot find it.
//
// This is also what makes docs/specs/29-first-run-admin-guidance.md's design
// checkable rather than merely intended. That spec routes an admin with no
// storage to the admin area through a server-side redirect, specifically so
// that no client-side link has to exist; a test that only looked at filenames
// would not notice the day someone adds the link anyway.
//
// The failure message quotes the path rather than the content on purpose: the
// tree contains PNGs, and a diff of one of those is not a readable test
// failure.
func TestNoStaticAssetNamesTheAdminArea(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)

	needle := []byte("/admin")
	var scanned int
	require.NoError(t, fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		scanned++
		assert.Falsef(t, bytes.Contains(body, needle),
			"%s contains %q: nothing served from web/static/ may name the admin area, "+
				"in code, copy or a comment (docs/specs/03-auth-and-multi-tenancy.md)", path, needle)
		return nil
	}))

	// A walk that silently visited nothing would pass this test while checking
	// nothing at all — the same failure mode TestStaticIsRootedAtStaticDir
	// guards against from the other side.
	assert.Greater(t, scanned, 1, "the walk must actually have read the embedded tree")
}
