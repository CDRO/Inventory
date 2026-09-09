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
