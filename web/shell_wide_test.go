package web_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/web"
)

// docs/specs/05-frontend-pwa-foundations.md's "Layout width" amendment makes
// .shell--wide every page's default on its <header>, <nav>, and <main> alike
// — "no exception list to maintain: a new page defaults to wide, full stop."
// That rule is checked today only by e2e/specs/start-page.spec.js's
// WIDE_PAGES list, and that suite runs via .github/workflows/e2e.yml on
// push-to-main / workflow_dispatch only — never a required PR check
// (docs/specs/01-architecture-and-deployment.md) — so a PR that quietly
// dropped shell--wide from one page, or added a new page without it, would
// pass the merge gate (#414).
//
// This test reads every shipped *.html page directly instead of hardcoding a
// page list: it follows whatever header/nav/main elements the page actually
// has (index.html has neither header nor nav; storages.html has no nav), so
// a new page is covered automatically rather than needing to be added to a
// list here, matching spec 05's own "no exception list" model.
var elementPattern = regexp.MustCompile(`<(header|nav|main)\b[^>]*>`)

var classAttrPattern = regexp.MustCompile(`class="([^"]*)"`)

func TestEveryPageUsesShellWide(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)

	entries, err := fs.ReadDir(assets, ".")
	require.NoError(t, err)

	var checked int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".html") {
			continue
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body, err := fs.ReadFile(assets, name)
			require.NoError(t, err)
			source := string(body)

			tags := elementPattern.FindAllStringSubmatch(source, -1)
			require.NotEmpty(t, tags, "%s has no <header>, <nav>, or <main> element to check", name)

			var sawMain bool
			for _, tag := range tags {
				element := tag[1]
				if element == "main" {
					sawMain = true
				}

				class := classAttrPattern.FindStringSubmatch(tag[0])
				require.NotNil(t, class, "%s's <%s> has no class attribute", name, element)

				classes := strings.Fields(class[1])
				assert.Contains(t, classes, "shell--wide",
					"%s's <%s> must carry shell--wide alongside shell (spec 05, \"Layout width\")",
					name, element)
			}

			assert.True(t, sawMain, "%s has no <main> element", name)
		})
		checked++
	}

	assert.Greater(t, checked, 5, "the walk must actually have read the page files")
}
