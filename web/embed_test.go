package web_test

import (
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
