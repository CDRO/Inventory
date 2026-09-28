package iconlib_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// iconFile is one source file in the picker's search, pick and
// alias-recording flow.
//
// mayImportNetHTTP marks the files that live in package httpapi, whose handler
// signatures are necessarily http.ResponseWriter/*http.Request. The import is
// unavoidable there, so those files are held to the narrower rule instead:
// they may name the package, but must not construct an outbound request with
// it.
type iconFile struct {
	path             string
	mayImportNetHTTP bool
}

// requiredIconFiles is the tripwire under the globs below: the files that
// implement this flow today. A rename that moves one out of the "icon*"
// convention would otherwise shrink the glob's result silently, leaving the
// test green over a file it no longer looks at.
var requiredIconFiles = []string{
	"iconlib.go",
	"../store/icons.go",
	"../store/icon_aliases.go",
	"../httpapi/icons.go",
	"../httpapi/iconsuggestions.go",
}

// iconSearchFiles globs the icon path rather than listing it, so that a file
// added to the flow later is covered the day it lands rather than the day
// someone remembers this test exists. review-go raised the fixed list as a
// silent-coverage-loss risk on PR #426 round 2: a rename is caught by
// requiredIconFiles above, but an *addition* to a hardcoded list is not caught
// by anything.
func iconSearchFiles(t *testing.T) []iconFile {
	t.Helper()

	groups := []struct {
		glob             string
		mayImportNetHTTP bool
	}{
		{glob: "*.go"},              // this package: the vendored icon set itself
		{glob: "../store/icon*.go"}, // icons.go, icon_aliases.go
		{glob: "../httpapi/icon*.go", mayImportNetHTTP: true},
	}

	var files []iconFile
	found := map[string]bool{}
	for _, group := range groups {
		matches, err := filepath.Glob(group.glob)
		require.NoError(t, err)

		for _, match := range matches {
			match = filepath.ToSlash(match)
			if strings.HasSuffix(match, "_test.go") {
				continue
			}
			files = append(files, iconFile{path: match, mayImportNetHTTP: group.mayImportNetHTTP})
			found[match] = true
		}
	}

	for _, required := range requiredIconFiles {
		require.Truef(t, found[required],
			"%s implements the icon picker's search path but this test no longer "+
				"sees it — if it was renamed, the \"icon*\" naming convention these "+
				"globs rely on has been broken, and the guard has quietly stopped "+
				"covering it. Rename it back or widen the globs; do not just update "+
				"requiredIconFiles.", required)
	}
	return files
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

const (
	modulePrefix = "github.com/CDRO/Inventory/"
	// The repository root, relative to this package's directory.
	repoRoot = "../../"
)

// packageReachesNetwork returns the import chain by which pkg reaches a
// network package, or nil if it cannot reach one. pkg is an import path inside
// this module; imports outside it are not followed.
//
// This is what closes the gap a direct-call check leaves open. Banning
// net/http in the icon path's own files says nothing about a call made one
// level down — and this repository already contains the perfect vehicle for
// exactly that: internal/imagesearch exposes Iconify.Candidates, a live search
// against api.iconify.design, wired up and working for spec 07. A single line
// in the icon handler,
//
//	imagesearch.NewIconify(nil).Candidates(r.Context(), query, limit)
//
// reintroduces the live icon search that spec 42 exists to remove, with no
// net/http import and no URL literal anywhere in the icon path. That is also
// the most *likely* shape for the regression, not a contrived one: a
// "restore the old search as a fallback" patch would reach for the client that
// is already there. It was found by review-tests probing this guard for
// evasions on PR #426 round 2, and it defeated the guard's first version.
func packageReachesNetwork(t *testing.T, pkg string, seen map[string]bool) []string {
	t.Helper()

	if seen[pkg] {
		return nil
	}
	seen[pkg] = true

	dir := filepath.Join(repoRoot, strings.TrimPrefix(pkg, modulePrefix))
	entries, err := os.ReadDir(dir)
	require.NoErrorf(t, err, "cannot read package %s at %s", pkg, dir)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)

		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if networkImports[path] {
				return []string{pkg, path}
			}
			if !strings.HasPrefix(path, modulePrefix) {
				continue
			}
			if chain := packageReachesNetwork(t, path, seen); chain != nil {
				return append([]string{pkg}, chain...)
			}
		}
	}
	return nil
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

	for _, file := range iconSearchFiles(t) {
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

			// The local name net/http is bound to in this file. An aliased
			// import (`import nethttp "net/http"`) would otherwise walk past
			// the outbound-call check below, which review-go raised on PR #426
			// round 2 — the check compared against the literal "http".
			httpName := ""

			for _, imp := range parsed.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if path == "net/http" && file.mayImportNetHTTP {
					httpName = "http"
					if imp.Name != nil {
						httpName = imp.Name.Name
					}
					continue
				}
				require.Falsef(t, networkImports[path],
					"%s imports %q. The icon picker's search, pick and alias-recording "+
						"must resolve entirely against this deployment's own database and "+
						"the vendored icon set (docs/specs/40-icon-picker.md, "+
						"docs/specs/42-local-icon-library.md).",
					file.path, path)

				if !strings.HasPrefix(path, modulePrefix) {
					continue
				}
				chain := packageReachesNetwork(t, path, map[string]bool{})
				require.Nilf(t, chain,
					"%s imports %q, which can reach the network: %s. The icon path may "+
						"not call out even indirectly — internal/imagesearch's Iconify "+
						"client is spec 07's image-suggestion path, and routing icon "+
						"search back through it is exactly the regression "+
						"docs/specs/42-local-icon-library.md exists to prevent.",
					file.path, path, strings.Join(chain, " -> "))
			}

			ast.Inspect(parsed, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.SelectorExpr:
					pkg, ok := node.X.(*ast.Ident)
					if !ok || httpName == "" || pkg.Name != httpName {
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

// stripLineComment drops a trailing // comment, leaving any "//" that is
// inside a string literal alone.
//
// Two different bugs have lived in this one function, both of which made a
// check vacuous while it still read as coverage — which is worse than having
// no check at all:
//
//   - strings.Cut(line, "//") cuts `const X = "https://api.iconify.design"`
//     at the scheme's own slashes and returns `const X = "https:`, containing
//     neither "http://" nor "https://". The first draft did this and passed
//     against a deliberately injected live call.
//   - Treating only a preceding ':' as "not a comment" fixes that case but
//     leaves the protocol-relative one dead: `const CDN = "//cdn.example/x"`
//     is still cut at the quote, so the `"//` check could never fire. Found
//     by review-go on PR #426 round 2.
//
// Tracking string state is what actually covers both, and is why this is a
// scanner rather than a search.
func stripLineComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		char := line[i]
		switch {
		case quote != 0:
			if char == '\\' {
				i++ // skip whatever this escapes, including a quote
			} else if char == quote {
				quote = 0
			}
		case char == '"' || char == '\'' || char == '`':
			quote = char
		case char == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}
