package web_test

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/web"
)

// TestI18nCatalogsHaveIdenticalKeys is the only mechanism that keeps a
// no-toolchain project's translations from rotting
// (docs/specs/19-localization.md): a key added to en.json and not de.json
// (or the reverse) must fail the build, not silently render the raw key or
// the wrong language to a user.
func TestI18nCatalogsHaveIdenticalKeys(t *testing.T) {
	t.Parallel()

	en := loadCatalog(t, "en")
	de := loadCatalog(t, "de")

	var missingFromDE, missingFromEN []string
	for key := range en {
		if _, ok := de[key]; !ok {
			missingFromDE = append(missingFromDE, key)
		}
	}
	for key := range de {
		if _, ok := en[key]; !ok {
			missingFromEN = append(missingFromEN, key)
		}
	}

	assert.Empty(t, missingFromDE, "keys present in en.json but missing from de.json: %v", missingFromDE)
	assert.Empty(t, missingFromEN, "keys present in de.json but missing from en.json: %v", missingFromEN)
}

// TestI18nCatalogsHaveMatchingPlaceholders asserts every {placeholder} in a
// key's en string also appears in its de string and vice versa — a
// translation that drops or renames a placeholder fails at runtime as a
// literal "{count}" left in the rendered string, which nothing else here
// would catch.
func TestI18nCatalogsHaveMatchingPlaceholders(t *testing.T) {
	t.Parallel()

	en := loadCatalog(t, "en")
	de := loadCatalog(t, "de")

	for key, enValue := range en {
		deValue, ok := de[key]
		if !ok {
			// Reported by TestI18nCatalogsHaveIdenticalKeys; this test only
			// checks placeholders for keys present in both.
			continue
		}

		enPlaceholders := placeholderSet(enValue)
		dePlaceholders := placeholderSet(deValue)

		assert.ElementsMatchf(t, keysOf(enPlaceholders), keysOf(dePlaceholders),
			"key %q: placeholders differ between en (%q) and de (%q)", key, enValue, deValue)
	}
}

// TestI18nCatalogsAreNonEmptyAndParse guards the two tests above from a
// silently-empty or unreadable catalog passing them for the wrong reason:
// two empty maps have identical (empty) key sets.
func TestI18nCatalogsAreNonEmptyAndParse(t *testing.T) {
	t.Parallel()

	for _, lang := range []string{"en", "de"} {
		t.Run(lang, func(t *testing.T) {
			t.Parallel()
			catalog := loadCatalog(t, lang)
			assert.Greater(t, len(catalog), 0, "%s.json must not be empty", lang)
		})
	}
}

func loadCatalog(t *testing.T, lang string) map[string]string {
	t.Helper()

	assets, err := web.Static()
	require.NoError(t, err)

	body, err := fs.ReadFile(assets, "i18n/"+lang+".json")
	require.NoErrorf(t, err, "web/static/i18n/%s.json must be embedded and readable", lang)

	var catalog map[string]string
	require.NoErrorf(t, json.Unmarshal(body, &catalog), "web/static/i18n/%s.json must parse as a flat string map", lang)

	return catalog
}

var placeholderPattern = regexp.MustCompile(`\{(\w+)\}`)

func placeholderSet(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, match := range placeholderPattern.FindAllStringSubmatch(s, -1) {
		set[match[1]] = struct{}{}
	}
	return set
}

func keysOf(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	return keys
}
