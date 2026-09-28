package iconlib_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The icon picker's search, pick and alias-recording flow, file by file.
//
// mayImportNetHTTP marks the two files that live in package httpapi, whose
// handler signatures are necessarily http.ResponseWriter/*http.Request. The
// import is unavoidable there, so those files are held to the narrower rule
// below instead: they may name the package, but must not construct an
// outbound request with it.
var iconSearchFiles = []struct {
	path             string
	mayImportNetHTTP bool
}{
	{path: "iconlib.go"},
	{path: "../store/icons.go"},
	{path: "../store/icon_aliases.go"},
	{path: "../httpapi/iconsuggestions.go", mayImportNetHTTP: true},
	{path: "../httpapi/icons.go", mayImportNetHTTP: true},
}

// Every net/http identifier that begins an outbound request. Naming them
// explicitly, rather than banning the import outright, is what lets the two
// handler files be covered by the same test as the rest.
var outboundHTTPIdents = map[string]string{
	"Get":                   "http.Get",
	"Post":                  "http.Post",
	"PostForm":              "http.PostForm",
	"Head":                  "http.Head",
	"NewRequest":            "http.NewRequest",
	"NewRequestWithContext": "http.NewRequestWithContext",
	"Client":                "http.Client",
	"DefaultClient":         "http.DefaultClient",
	"Transport":             "http.Transport",
	"DefaultTransport":      "http.DefaultTransport",
}

// Import paths that can only be there to talk to something off-box. net/http
// is handled per-file above; these are banned everywhere in the icon path.
var networkImports = map[string]bool{
	"net":          true,
	"net/http":     true, // only reachable for files without mayImportNetHTTP
	"net/http/cgi": true,
	"net/rpc":      true,
	"net/smtp":     true,
}

// urlPattern finds each absolute URL inside a string literal. Literals here
// are not bare URLs — iconlib.go's is a whole SVG wrapper with the namespace
// embedded in it — so the check has to match occurrences rather than test the
// literal as a whole.
var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'` + "`" + `<>]*`)

// xmlNamespacePrefix is the one "://" this code legitimately contains: the SVG
// namespace in iconlib.go's wrapper markup. It is an identifier in the XML
// sense and is never dereferenced — no renderer fetches it — so it is
// exempted by name rather than by relaxing the URL rule.
const xmlNamespacePrefix = "http://www.w3.org/"

// TestIconSearchNeverLeavesTheDeployment is spec 40's network-egress
// acceptance criterion as an automated regression guard: "No request this
// spec's endpoints make ever reaches a host outside this deployment — verify
// by running the picker with network egress blocked (or a proxy that fails
// every external request) and confirming search, pick, and alias-recording
// all still work." Spec 42 names the same check as its own regression guard,
// and issues #401 and #405 both called for it by name.
//
// This is deliberately a repository-level property test rather than a browser
// or network-isolation one, for the same reason
// web/shell_manifest_test.go's own comment gives: the property that matters is
// about the source ("does the icon path contain code that could call out"),
// not about one runtime observation. A browser-level egress block would not
// even test the right thing here — the hypothetical call is made by the
// *server*, so blocking the page's network proves nothing about it, and the
// E2E stack has ordinary internet access throughout.
//
// The gap this closes is specific and was found in review of PR #426: swapping
// internal/httpapi/iconsuggestions.go's two local SELECTs for a call to
// api.iconify.design left every existing test — unit, handler and E2E — green.
// That matters more here than it would for most features, because the local
// design is not an implementation detail but a direction Tizian reversed by
// hand mid-review (spec 42's "Why this spec exists"): "I do not want the
// inventory to make an external call to iconify to search for icons... I
// already have two dependencies, I do not want or need a third one." Without
// this test, a later "restore the old search as a fallback" patch, or a bad
// merge, would ship green.
//
// Note what this does NOT claim. It is a guard on the icon path only, and says
// nothing about internal/imagesearch, which legitimately calls
// api.iconify.design for spec 07's image-suggestion feature — a different
// feature, with a different and still-current decision behind it.
func TestIconSearchNeverLeavesTheDeployment(t *testing.T) {
	t.Parallel()

	for _, file := range iconSearchFiles {
		t.Run(file.path, func(t *testing.T) {
			t.Parallel()

			src, err := os.ReadFile(file.path)
			require.NoErrorf(t, err, "icon-path file %s is listed in this test but missing — if it moved, move the entry too rather than dropping it", file.path)

			fset := token.NewFileSet()
			// Comments are not in the walked AST, which is what lets
			// iconlib.go keep its doc-comment references to Iconify's
			// *format* (and its documentation URL) without tripping this.
			parsed, err := parser.ParseFile(fset, file.path, src, 0)
			require.NoError(t, err)

			for _, imp := range parsed.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if path == "net/http" && file.mayImportNetHTTP {
					continue
				}
				require.Falsef(t, networkImports[path],
					"%s imports %q. The icon picker's search, pick and alias-recording "+
						"must resolve entirely against this deployment's own database and "+
						"the vendored icon set (docs/specs/40-icon-picker.md, "+
						"docs/specs/42-local-icon-library.md).",
					file.path, path)
			}

			ast.Inspect(parsed, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.SelectorExpr:
					pkg, ok := node.X.(*ast.Ident)
					if !ok || pkg.Name != "http" {
						return true
					}
					if name, banned := outboundHTTPIdents[node.Sel.Name]; banned {
						t.Errorf("%s:%d uses %s. The icon path must not start an "+
							"outbound request: spec 40 requires that no request its "+
							"endpoints make reaches a host outside this deployment.",
							file.path, fset.Position(node.Pos()).Line, name)
					}
				case *ast.BasicLit:
					if node.Kind != token.STRING {
						return true
					}
					for _, url := range urlPattern.FindAllString(node.Value, -1) {
						if strings.HasPrefix(url, xmlNamespacePrefix) {
							continue
						}
						t.Errorf("%s:%d contains the URL %q. Every icon the picker "+
							"serves comes from this database or the vendored set in "+
							"internal/iconlib/data (docs/specs/42-local-icon-library.md); "+
							"nothing in this path may name an external host.",
							file.path, fset.Position(node.Pos()).Line, url)
					}
				}
				return true
			})
		})
	}
}

// TestIconPickerFrontendCallsOnlyItsOwnAPI is the browser half of the same
// criterion. The picker's JavaScript is the other place a live icon search
// could be reintroduced — and the easier one, since it needs no Go change at
// all — so a page that fetched api.iconify.design directly would satisfy the
// test above while still breaking the spec.
//
// Vanilla JS with no build step means there is no import graph to walk, so
// this reads the file. It is a coarser check than the Go one and deliberately
// so: any absolute URL in this file is wrong, because every endpoint the
// picker talks to is same-origin.
func TestIconPickerFrontendCallsOnlyItsOwnAPI(t *testing.T) {
	t.Parallel()

	const pickerPath = "../../web/static/js/icon-picker.js"

	src, err := os.ReadFile(pickerPath)
	require.NoErrorf(t, err, "%s is listed in this test but missing — if it moved, move the entry too rather than dropping it", pickerPath)

	for i, line := range strings.Split(string(src), "\n") {
		code := stripLineComment(line)
		// "//" on its own covers a protocol-relative URL (//cdn.example/x),
		// which is how a CDN reference is most often written; requiring the
		// opening quote keeps it from matching an ordinary comment.
		for _, forbidden := range []string{"http://", "https://", `"//`, "'//"} {
			require.NotContainsf(t, code, forbidden,
				"%s:%d reaches an absolute URL. The icon picker is served by, and "+
					"talks only to, this deployment (docs/specs/40-icon-picker.md's "+
					"network-egress criterion); libraries are vendored as single "+
					"files, never loaded from a CDN (CLAUDE.md).",
				pickerPath, i+1)
		}
	}
}

// stripLineComment drops a trailing // comment, leaving a URL's own "//"
// alone.
//
// The naive strings.Cut(line, "//") is wrong here in a way that matters: it
// cuts `const X = "https://api.iconify.design"` at the scheme's own slashes
// and hands back `const X = "https:`, which contains neither "http://" nor
// "https://". The first draft of this test did exactly that and passed
// against a deliberately injected live call — a vacuous guard is worse than
// none, because it reads as coverage.
func stripLineComment(line string) string {
	for i := 0; i+1 < len(line); i++ {
		if line[i] != '/' || line[i+1] != '/' {
			continue
		}
		if i > 0 && line[i-1] == ':' {
			continue // part of a scheme, not the start of a comment
		}
		return line[:i]
	}
	return line
}
