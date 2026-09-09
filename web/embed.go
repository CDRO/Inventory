// Package web ships the browser-facing assets. The files under static/ are
// served to the browser byte-for-byte as they exist in the repository — there
// is no build step, bundler, or transpiler anywhere in the frontend
// (docs/specs/05-frontend-pwa-foundations.md).
//
// They are compiled into the binary so a production deployment is a single
// artifact with nothing to mount. Setting STATIC_DIR makes the server read the
// same tree from disk instead, which is what lets a dev edit a .js file and
// just refresh the browser.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// Static returns the embedded asset tree rooted at web/static, so that
// static/index.html is served as /index.html.
func Static() (fs.FS, error) {
	return fs.Sub(embedded, "static")
}
