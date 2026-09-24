package web_test

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/web"
)

// The navigation bar's wiring (docs/specs/34-navigation-and-start-page.md).
//
// web/static/js/nav.js is one implementation of the bar, but every page still
// has to say which entry is its own: `renderNav(container, { current })`. That
// one argument is hand-written in eleven files and is the only thing deciding
// whether a page's own entry gets `aria-current="page"`.
//
// Getting it wrong is silent. A page passing `"shopping-list"` where nav.js
// spells the key `"shopping_list"`, or a copy-paste leaving products.js on
// `"categories"`, leaves the bar rendering perfectly with no entry marked at
// all — or, worse, the wrong one marked. Nothing about the page looks broken.
//
// So the wiring is checked here rather than page by page in a browser: the
// keys are parsed out of the shipped assets and cross-checked, which is one
// test in the merge gate instead of eleven E2E journeys that would each have
// to be remembered when a page is added.

// navKeyPattern matches `key: "dashboard"` inside nav.js's NAV_ITEMS.
var navKeyPattern = regexp.MustCompile(`key:\s*"(\w+)"`)

// extraCurrentPattern matches the `current === "inbox"` comparisons in
// renderNav. Inbox and Settings are appended outside NAV_ITEMS — the inbox
// entry because its badge comes from js/inbox-badge.js, Settings because it
// sits in the end group — so their keys are read from the comparisons that
// actually consume them rather than repeated here, where a typo in nav.js
// would simply be copied into the test.
var extraCurrentPattern = regexp.MustCompile(`current === "(\w+)"`)

// pageCurrentPattern matches `current: "locations",` in a page module's
// renderNav call.
//
// The character class allows a hyphen even though no valid key contains one,
// so that `"shopping-list"` — the exact near-miss this test exists to catch,
// since the page's file is hyphenated and the key is not — is captured and
// reported as the wrong key it is, rather than not matching at all and being
// reported as a page that names no entry.
var pageCurrentPattern = regexp.MustCompile(`current:\s*"([\w-]+)"`)

// pagesWithNoEntryOfTheirOwn are the storage-scoped pages that render the bar
// but deliberately mark nothing: the two review screens are reached from the
// inbox and are not destinations in the bar, so `current` is absent and no
// entry carries `aria-current` (docs/specs/34-navigation-and-start-page.md).
var pagesWithNoEntryOfTheirOwn = map[string]bool{
	"review.js":         true,
	"consume-review.js": true,
}

// TestEveryPageWiresItsOwnNavigationEntry cross-checks each page module's
// `current` key against nav.js's own vocabulary, and against the page's own
// name.
//
// The name rule is what makes this more than a spell-check: a page's key must
// be its own filename with hyphens as underscores. `shopping-list.js` must say
// `shopping_list`, and a products page that said `categories` would fail even
// though `categories` is a perfectly valid key.
func TestEveryPageWiresItsOwnNavigationEntry(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)

	nav, err := fs.ReadFile(assets, "js/nav.js")
	require.NoError(t, err, "web/static/js/nav.js must be embedded")

	known := map[string]bool{}
	for _, match := range navKeyPattern.FindAllStringSubmatch(string(nav), -1) {
		known[match[1]] = true
	}
	for _, match := range extraCurrentPattern.FindAllStringSubmatch(string(nav), -1) {
		known[match[1]] = true
	}
	// A parse that found nothing would pass every assertion below while
	// checking nothing at all — the same failure mode the catalog tests guard
	// against from the other side.
	require.NotEmpty(t, known, "no navigation keys parsed out of nav.js")
	require.Contains(t, known, "inbox", "the inbox entry's key must be readable from renderNav")
	require.Contains(t, known, "settings", "the settings entry's key must be readable from renderNav")

	entries, err := fs.ReadDir(assets, "js/pages")
	require.NoError(t, err)

	var checked int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".js") {
			continue
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body, err := fs.ReadFile(assets, path.Join("js/pages", name))
			require.NoError(t, err)
			source := string(body)

			if !strings.Contains(source, "renderNav(") {
				// index.html and storages.html render no bar: neither has a
				// resolved storage to build one around.
				return
			}

			found := pageCurrentPattern.FindAllStringSubmatch(source, -1)
			if pagesWithNoEntryOfTheirOwn[name] {
				assert.Empty(t, found,
					"%s is reached from the inbox, not from the bar, so it marks no entry", name)
				return
			}

			require.Len(t, found, 1,
				"%s renders the bar, so it must name its own entry exactly once", name)
			key := found[0][1]

			want := strings.ReplaceAll(strings.TrimSuffix(name, ".js"), "-", "_")
			assert.Equal(t, want, key,
				"%s must mark its own entry: a page whose key names another page leaves the "+
					"wrong entry highlighted, and one whose key names nothing leaves the bar unmarked", name)
			assert.Contains(t, known, key,
				"%s passes a key nav.js does not know, so no entry is marked at all", name)
		})
		checked++
	}

	assert.Greater(t, checked, 5, "the walk must actually have read the page modules")
}
