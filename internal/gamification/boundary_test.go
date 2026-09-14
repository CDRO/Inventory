package gamification_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// allowedImporters names packages outside internal/gamification that may
// legitimately depend on it. internal/store is here as of spec 51
// (docs/specs/51-gamification-scoring.md): it computes XP using this
// package's named constants and level formula, and writes the results
// through the progress API routes it exposes. Add a new entry only for a
// deliberate, reviewed integration point — never to silence this test.
var allowedImporters = map[string]bool{
	"github.com/CDRO/Inventory/internal/store": true,
}

// modulePath is read from `go list -m` rather than hardcoded, so a module
// rename cannot silently turn this test into a vacuous pass (it would instead
// fail the `go list` call below with an unrelated-looking error, which is
// still safer than passing for the wrong reason).
func modulePath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// gamificationImportPath is this module's import path for the package under
// test, kept as one function so the module name is not hardcoded twice.
func gamificationImportPath(t *testing.T) string {
	return modulePath(t) + "/internal/gamification"
}

// TestNothingCoreDependsOnGamification enforces the phase boundary in
// docs/specs/50-gamification-overview.md: nothing in specs 00-11 (outside
// the allowlist above) may *directly* import internal/gamification.
//
// This checks direct imports (go list's "Imports", "TestImports" and
// "XTestImports" fields) rather than the full transitive dependency closure
// ("Deps") deliberately: once internal/store legitimately imports
// gamification, every package that imports store — which is most of the
// application — transitively "depends" on it too, exactly as it transitively
// depends on pgx or chi. That is ordinary layering, not the coupling this
// test exists to catch. What must never happen is a package reaching past
// store to import gamification's constants or scoring functions directly,
// which is what would actually make gamification something specs 00-11 rely
// on rather than something store happens to be built with.
//
// This walks `go list -json` over the whole module rather than a
// hand-maintained file list, so a new package added anywhere is checked
// automatically. It lists the module's import-path pattern (modulePath+"/...")
// rather than the relative "./..." — `go test` runs this package's test
// binary with its working directory set to internal/gamification itself, so
// a relative "./..." here would only ever see this package, never the rest
// of the module.
func TestNothingCoreDependsOnGamification(t *testing.T) {
	module := modulePath(t)
	gamificationPath := gamificationImportPath(t)

	out, err := exec.Command("go", "list", "-json", module+"/...").Output()
	if err != nil {
		t.Fatalf("go list -json %s/...: %v", module, err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath   string
			Imports      []string
			TestImports  []string
			XTestImports []string
		}
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode go list output: %v", err)
		}

		if pkg.ImportPath == gamificationPath ||
			strings.HasPrefix(pkg.ImportPath, gamificationPath+"/") {
			continue // gamification's own package and subpackages are not the violation
		}
		if allowedImporters[pkg.ImportPath] {
			continue
		}

		for _, imports := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
			for _, dep := range imports {
				if dep == gamificationPath {
					t.Errorf("%s imports %s directly — specs 00-11 must stay shippable with the "+
						"entire 50+ range unimplemented (docs/specs/50-gamification-overview.md); "+
						"add it to allowedImporters only for a deliberate, reviewed integration point",
						pkg.ImportPath, gamificationPath)
				}
			}
		}
	}
}
