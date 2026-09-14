package gamification_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// modulePath is this project's module path, from go.mod.
const modulePath = "github.com/CDRO/Inventory"

// gamificationImportPath is this module's import path for the package under
// test, kept as one constant so the module name is not hardcoded twice.
const gamificationImportPath = modulePath + "/internal/gamification"

// allowedImporters names packages outside internal/gamification that may
// legitimately depend on it. Empty today: nothing in specs 00-11 needs
// gamification yet (docs/specs/50-gamification-overview.md). Spec 51 will
// add real integration points (progress API routes, an XP increment beside
// an inventory write) — when it does, add the specific package here rather
// than deleting this test, so an *undeclared* new dependency elsewhere still
// fails the build.
var allowedImporters = map[string]bool{}

// TestNothingCoreDependsOnGamification enforces the phase boundary in
// docs/specs/50-gamification-overview.md: the inventory system in specs
// 00-11 must stay complete, correct, and shippable with the entire 50+
// range unimplemented, which requires that nothing outside it (and outside
// the allowlist above) imports it.
//
// This walks `go list -json` over the whole module rather than a
// hand-maintained file list, so a new package added anywhere is checked
// automatically. It lists the module's import-path pattern (modulePath+"/...")
// rather than the relative "./..." — `go test` runs this package's test
// binary with its working directory set to internal/gamification itself, so
// a relative "./..." here would only ever see this package, never the rest
// of the module.
func TestNothingCoreDependsOnGamification(t *testing.T) {
	out, err := exec.Command("go", "list", "-json", modulePath+"/...").Output()
	if err != nil {
		t.Fatalf("go list -json %s/...: %v", modulePath, err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath   string
			Deps         []string
			TestImports  []string
			XTestImports []string
		}
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode go list output: %v", err)
		}

		if pkg.ImportPath == gamificationImportPath ||
			strings.HasPrefix(pkg.ImportPath, gamificationImportPath+"/") {
			continue // gamification's own package and subpackages are not the violation
		}
		if allowedImporters[pkg.ImportPath] {
			continue
		}

		for _, imports := range [][]string{pkg.Deps, pkg.TestImports, pkg.XTestImports} {
			for _, dep := range imports {
				if dep == gamificationImportPath {
					t.Errorf("%s imports %s — specs 00-11 must stay shippable with the "+
						"entire 50+ range unimplemented (docs/specs/50-gamification-overview.md); "+
						"add it to allowedImporters only for a deliberate, reviewed integration point",
						pkg.ImportPath, gamificationImportPath)
				}
			}
		}
	}
}
