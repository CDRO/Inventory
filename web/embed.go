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

// templates/ is a separate embed, and deliberately not under static/.
//
// The admin UI is server-rendered (docs/specs/03-auth-and-multi-tenancy.md),
// and the rule is that nothing admin-shaped ships in the JavaScript
// application. A template sitting under static/ would be served to anyone who
// asked for it by path — no data in it, but a complete map of the admin area
// handed to every browser. Keeping the two trees apart is what makes "the
// admin UI does not exist in the frontend" true of the files, not just of the
// routes.
//
//go:embed templates
var templates embed.FS

// Static returns the embedded asset tree rooted at web/static, so that
// static/index.html is served as /index.html.
func Static() (fs.FS, error) {
	return fs.Sub(embedded, "static")
}

// Templates returns the server-side templates rooted at web/templates.
func Templates() (fs.FS, error) {
	return fs.Sub(templates, "templates")
}
