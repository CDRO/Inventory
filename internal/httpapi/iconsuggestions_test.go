package httpapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// docs/specs/40-icon-picker.md.

func TestIconSuggestRequiresQuery(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/icon-suggestions", "")

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "query")
}

func TestIconSuggestRanksAliasHitsBeforeNameHits(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.icons.aliasHits = []store.IconSuggestion{
		{IconName: "noto:beer-mug", Source: store.IconSourceVendored, SVGBody: "<svg>alias</svg>", MatchedAlias: "beer"},
	}
	f.icons.nameHits = []store.IconSuggestion{
		{IconName: "noto:root-beer", Source: store.IconSourceVendored, SVGBody: "<svg>name</svg>"},
	}

	rec := f.do(http.MethodGet, f.base()+"/icon-suggestions?query=beer", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Suggestions []struct {
			IconName     string `json:"icon_name"`
			MatchedAlias string `json:"matched_alias"`
		} `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Suggestions, 2)
	assert.Equal(t, "noto:beer-mug", body.Suggestions[0].IconName)
	assert.Equal(t, "beer", body.Suggestions[0].MatchedAlias)
	assert.Equal(t, "noto:root-beer", body.Suggestions[1].IconName)
	assert.Empty(t, body.Suggestions[1].MatchedAlias)
}

func TestIconSuggestDedupesNameHitAlreadyPresentAsAliasHit(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.icons.aliasHits = []store.IconSuggestion{
		{IconName: "noto:beer-mug", Source: store.IconSourceVendored, SVGBody: "<svg/>", MatchedAlias: "beer"},
	}
	// The same icon also happens to match the direct-name tier — must not
	// appear twice.
	f.icons.nameHits = []store.IconSuggestion{
		{IconName: "noto:beer-mug", Source: store.IconSourceVendored, SVGBody: "<svg/>"},
	}

	rec := f.do(http.MethodGet, f.base()+"/icon-suggestions?query=beer", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Suggestions []struct {
			IconName string `json:"icon_name"`
		} `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Len(t, body.Suggestions, 1)
}

func TestIconSuggestInlinesSVGForVendoredAndPointsUploadedAtServeSVG(t *testing.T) {
	t.Parallel()

	uploadedID := uuid.New()
	f := newAPIFixture(t)
	f.icons.nameHits = []store.IconSuggestion{
		{IconName: "noto:vendored-thing", Source: store.IconSourceVendored, SVGBody: "<svg>trusted</svg>"},
		{IconName: "custom:uploaded-thing", Source: store.IconSourceUploaded, IconID: uploadedID, SVGBody: "<svg>untrusted</svg>"},
	}

	rec := f.do(http.MethodGet, f.base()+"/icon-suggestions?query=thing", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Suggestions []struct {
			IconName string     `json:"icon_name"`
			Source   string     `json:"source"`
			SVGBody  string     `json:"svg_body"`
			IconID   *uuid.UUID `json:"icon_id"`
		} `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Suggestions, 2)

	vendored, uploaded := body.Suggestions[0], body.Suggestions[1]
	assert.Equal(t, "<svg>trusted</svg>", vendored.SVGBody, "a vendored icon's svg_body is inlined directly")
	assert.Nil(t, vendored.IconID)

	assert.Empty(t, uploaded.SVGBody, "an uploaded icon's body is never inlined — untrusted, person-supplied content")
	require.NotNil(t, uploaded.IconID)
	assert.Equal(t, uploadedID, *uploaded.IconID)
}

func TestIconSuggestEmptyResultIsAnEmptyListNotAFailure(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodGet, f.base()+"/icon-suggestions?query=nothing-matches-this", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Suggestions []any `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.Suggestions)
}

func TestCreateIconAliasRequiresNonEmptyFields(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/icon-suggestions/aliases", `{"icon_name":"","alias":""}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	fields := errorFields(t, rec)
	assert.Contains(t, fields, "icon_name")
	assert.Contains(t, fields, "alias")
}

func TestCreateIconAliasRejectsOver100Chars(t *testing.T) {
	t.Parallel()

	long := ""
	for i := 0; i < 101; i++ {
		long += "a"
	}

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/icon-suggestions/aliases", `{"icon_name":"`+long+`","alias":"beer"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, errorFields(t, rec), "icon_name")
}

func TestCreateIconAliasStoresTheSearchTermAgainstTheIcon(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/icon-suggestions/aliases", `{"icon_name":"noto:beer-mug","alias":"beer"}`)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "noto:beer-mug", f.icons.lastAliasFor)
	assert.Equal(t, "beer", f.icons.lastAlias)
	assert.Equal(t, f.user.ID, f.icons.lastAliasBy)
}

func TestCreateIconAliasIsNotAdminGated(t *testing.T) {
	t.Parallel()

	// Any authenticated storage member may record an alias
	// (docs/specs/40-icon-picker.md): additive, low-risk shared data, not an
	// admin action. f.user is an ordinary member, not an admin, and the
	// request still succeeds.
	f := newAPIFixture(t)
	rec := f.do(http.MethodPost, f.base()+"/icon-suggestions/aliases", `{"icon_name":"noto:beer-mug","alias":"beer"}`)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestIconSuggestStoreFailureIsInternalError(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.icons.aliasErr = errors.New("boom")

	rec := f.do(http.MethodGet, f.base()+"/icon-suggestions?query=beer", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
}
