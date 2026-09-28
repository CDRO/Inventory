package iconlib

import (
	"strings"
	"testing"
)

func TestNoto_NamesAreNamespacedAndMatchVendoredKeys(t *testing.T) {
	icons, err := Noto()
	if err != nil {
		t.Fatalf("Noto() error: %v", err)
	}
	if len(icons) == 0 {
		t.Fatal("Noto() returned no icons")
	}

	seen := make(map[string]bool, len(icons))
	for _, icon := range icons {
		if !strings.HasPrefix(icon.Name, "noto:") {
			t.Fatalf("icon name %q is not prefixed noto:", icon.Name)
		}
		key := strings.TrimPrefix(icon.Name, "noto:")
		if key == "" {
			t.Fatalf("icon name %q has an empty key", icon.Name)
		}
		if seen[icon.Name] {
			t.Fatalf("duplicate icon name %q", icon.Name)
		}
		seen[icon.Name] = true

		if !strings.HasPrefix(icon.SVGBody, "<svg ") {
			t.Fatalf("icon %q does not start with an <svg> element: %.80s", icon.Name, icon.SVGBody)
		}
		if !strings.Contains(icon.SVGBody, `viewBox="`) {
			t.Fatalf("icon %q has no viewBox", icon.Name)
		}
	}
}

// TestNoto_SpotCheckKnownIcon spot-checks one icon this collection is known
// to carry, per docs/specs/42-local-icon-library.md's acceptance criteria
// ("spot-check a handful against internal/iconlib/data/noto.json directly").
func TestNoto_SpotCheckKnownIcon(t *testing.T) {
	icons, err := Noto()
	if err != nil {
		t.Fatalf("Noto() error: %v", err)
	}

	byName := make(map[string]Icon, len(icons))
	for _, icon := range icons {
		byName[icon.Name] = icon
	}

	for _, name := range []string{"noto:red-heart", "noto:cheese-wedge"} {
		icon, ok := byName[name]
		if !ok {
			t.Fatalf("expected icon %q not found in the parsed collection", name)
		}
		if icon.SVGBody == "" {
			t.Fatalf("icon %q has an empty body", name)
		}
	}
}

// TestNoto_Deterministic guards the sort in parse(): two calls must produce
// identical output, which is what makes `inventory icons import` insert the
// same rows regardless of Go's map iteration order.
func TestNoto_Deterministic(t *testing.T) {
	first, err := Noto()
	if err != nil {
		t.Fatalf("Noto() error: %v", err)
	}
	second, err := Noto()
	if err != nil {
		t.Fatalf("Noto() error: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("call counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("icon %d differs between calls: %+v vs %+v", i, first[i], second[i])
		}
	}
}
