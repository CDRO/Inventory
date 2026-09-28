package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/iconlib"
	"github.com/CDRO/Inventory/internal/store"
)

// docs/specs/40-icon-picker.md, against a real database.

func seedIcon(t *testing.T, ctx context.Context, s *store.Store, name string) {
	t.Helper()
	_, err := s.ImportIcons(ctx, []iconlib.Icon{
		{Name: name, SVGBody: `<svg xmlns="http://www.w3.org/2000/svg"><path d="M1"/></svg>`},
	})
	require.NoError(t, err)
}

func TestSearchIconAliasesFindsAnAliasHit(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)
	name := "noto:test-beer-" + randomSuffix()

	seedIcon(t, ctx, s, name)
	require.NoError(t, s.CreateIconAlias(ctx, name, "beer", user))

	hits, err := s.SearchIconAliases(ctx, "beer", 20)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, name, hits[0].IconName)
	assert.Equal(t, "beer", hits[0].MatchedAlias)
	assert.NotEmpty(t, hits[0].SVGBody)
}

func TestSearchIconAliasesDedupesByIconName(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)
	name := "noto:test-drink-" + randomSuffix()

	seedIcon(t, ctx, s, name)
	require.NoError(t, s.CreateIconAlias(ctx, name, "drink", user))
	require.NoError(t, s.CreateIconAlias(ctx, name, "drank", user))

	hits, err := s.SearchIconAliases(ctx, "drink", 20)
	require.NoError(t, err)

	seen := map[string]int{}
	for _, h := range hits {
		seen[h.IconName]++
	}
	assert.LessOrEqual(t, seen[name], 1, "one icon_name must appear at most once, regardless of how many aliases match")
}

// TestSearchIconAliasesExcludesRenamedOrMissingIcon guards
// docs/specs/40-icon-picker.md's deliberate lack of a foreign key from
// icon_aliases.icon_name to icons.name: an alias left behind by a renamed or
// removed icon must degrade to "one fewer search hit," never a result with
// nothing to render.
func TestSearchIconAliasesExcludesRenamedOrMissingIcon(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)
	orphanTerm := "orphan-" + randomSuffix()

	require.NoError(t, s.CreateIconAlias(ctx, "noto:does-not-exist-"+randomSuffix(), orphanTerm, user))

	hits, err := s.SearchIconAliases(ctx, orphanTerm, 20)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func TestSearchIconsByNameFindsADirectHit(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	name := "noto:test-cheese-" + randomSuffix()
	seedIcon(t, ctx, s, name)

	hits, err := s.SearchIconsByName(ctx, "cheese", 20)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, name, hits[0].IconName)
	assert.Empty(t, hits[0].MatchedAlias, "a direct name hit carries no matched alias")
}

func TestCreateIconAliasIsIdempotent(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)
	name := "noto:test-milk-" + randomSuffix()
	seedIcon(t, ctx, s, name)

	require.NoError(t, s.CreateIconAlias(ctx, name, "milk", user))
	require.NoError(t, s.CreateIconAlias(ctx, name, "milk", user))

	count := countRows(t, ctx, `SELECT count(*) FROM icon_aliases WHERE icon_name = $1 AND alias = $2`, name, "milk")
	assert.Equal(t, 1, count, "ON CONFLICT (icon_name, alias) DO NOTHING: recording the same pair twice must insert nothing new")
}
