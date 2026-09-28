package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/iconlib"
	"github.com/CDRO/Inventory/internal/store"
)

// docs/specs/42-local-icon-library.md, against a real database.

// seedIconUploader creates a user to satisfy icons.created_by, for the
// uploaded-icon tests below — CreateIcon itself takes no storage, since an
// icon is global (docs/specs/02-data-model.md's catalog_products exception).
func seedIconUploader(t *testing.T, ctx context.Context, s *store.Store) uuid.UUID {
	t.Helper()
	user, err := s.CreateUser(ctx, store.NewUser{
		Username: "icon-uploader-" + randomSuffix(), PasswordHash: "x", DisplayName: "Icon Uploader",
	})
	require.NoError(t, err)
	return user.ID
}

func TestImportIconsIsIdempotent(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	icons := []iconlib.Icon{
		{Name: "noto:test-apple", SVGBody: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><path d="M1"/></svg>`},
		{Name: "noto:test-banana", SVGBody: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><path d="M2"/></svg>`},
	}

	inserted, err := s.ImportIcons(ctx, icons)
	require.NoError(t, err)
	assert.Equal(t, 2, inserted)

	// A second run changes nothing and does not error — the acceptance
	// criterion `inventory icons import` itself rests on.
	inserted, err = s.ImportIcons(ctx, icons)
	require.NoError(t, err)
	assert.Equal(t, 0, inserted, "ON CONFLICT (name) DO NOTHING: re-running must insert nothing new")

	count := countRows(t, ctx, `SELECT count(*) FROM icons WHERE name IN ($1, $2)`,
		"noto:test-apple", "noto:test-banana")
	assert.Equal(t, 2, count)
}

func TestImportIconsSetsSourceVendored(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.ImportIcons(ctx, []iconlib.Icon{
		{Name: "noto:test-cherry", SVGBody: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><path d="M3"/></svg>`},
	})
	require.NoError(t, err)

	var source string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT source FROM icons WHERE name = $1`, "noto:test-cherry").Scan(&source))
	assert.Equal(t, store.IconSourceVendored, source)
}

func TestCreateIconRejectsNameCollisionWithVendoredIcon(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)

	_, err := s.ImportIcons(ctx, []iconlib.Icon{
		{Name: "noto:test-grape", SVGBody: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><path d="M4"/></svg>`},
	})
	require.NoError(t, err)

	_, err = s.CreateIcon(ctx, store.NewIcon{
		Name:      "noto:test-grape",
		SVGBody:   `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`,
		CreatedBy: user,
	})
	require.ErrorIs(t, err, store.ErrValidation)
}

func TestCreateIconRejectsNameCollisionWithAnotherUpload(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)

	_, err := s.CreateIcon(ctx, store.NewIcon{
		Name:      "custom:my-thing",
		SVGBody:   `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`,
		CreatedBy: user,
	})
	require.NoError(t, err)

	_, err = s.CreateIcon(ctx, store.NewIcon{
		Name:      "custom:my-thing",
		SVGBody:   `<svg xmlns="http://www.w3.org/2000/svg"><circle/></svg>`,
		CreatedBy: user,
	})
	require.ErrorIs(t, err, store.ErrValidation)
}

func TestCreateIconThenIconSVGRoundTrips(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	user := seedIconUploader(t, ctx, s)

	body := `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`
	icon, err := s.CreateIcon(ctx, store.NewIcon{
		Name:      "custom:round-trip",
		SVGBody:   body,
		CreatedBy: user,
	})
	require.NoError(t, err)
	assert.Equal(t, store.IconSourceUploaded, icon.Source)

	loaded, err := s.IconSVG(ctx, icon.ID)
	require.NoError(t, err)
	assert.Equal(t, body, loaded)
}

func TestIconSVGUnknownIDIsNotFound(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	_, err := s.IconSVG(ctx, uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, store.ErrNotFound)
}
