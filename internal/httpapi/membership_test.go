package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/web"
)

// PATCH /api/storages/{storage_id}/membership and the start_page half of
// GET /api/auth/me (docs/specs/34-navigation-and-start-page.md).
//
// The claim worth testing is not "a select box saves". It is that a
// per-storage personal preference cannot be read across members and cannot be
// written for anyone but the caller: a join written the obvious way returns
// whichever member's row it happened to reach, and a handler that read a user
// id out of the body would honour one.

// SetStartPage writes the caller's row in the fake.
//
// f.startPages is the storage_members row for this purpose, so an absent key
// is "no such row" and the answer is store.ErrNotFound — exactly what the real
// UPDATE's zero rows-affected means. It never creates one: an upsert here
// would hide the case the handler has a branch for.
func (f *fakeAuth) SetStartPage(_ context.Context, storageID, userID uuid.UUID, startPage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := storageID.String() + userID.String()
	if _, ok := f.startPages[key]; !ok {
		return store.ErrNotFound
	}
	f.startPages[key] = startPage
	return nil
}

// startPageOf reads one member's stored value straight out of the fake, so a
// test can assert about a row no response ever shows it.
func (f *fakeAuth) startPageOf(storageID, userID uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startPages[storageID.String()+userID.String()]
}

// dropMembershipRow deletes the storage_members row while leaving the
// membership the gate consults intact. That combination is not a state the
// database can be in; it is how a test reaches the handler's own
// revoked-in-flight branch, which the gate would otherwise answer first.
func (f *fakeAuth) dropMembershipRow(storageID, userID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.startPages, storageID.String()+userID.String())
}

// storageEntry is one element of GET /api/auth/me's storages array.
type storageEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StartPage string `json:"start_page"`
}

func meStorages(t *testing.T, body *bytes.Buffer) []storageEntry {
	t.Helper()
	var decoded struct {
		Storages []storageEntry `json:"storages"`
	}
	require.NoError(t, json.Unmarshal(body.Bytes(), &decoded))
	return decoded.Storages
}

// TestPatchMembershipStoresTheChosenStartPage is the happy path: the value is
// stored, and echoed back so a client never has to guess what was kept.
func TestPatchMembershipStoresTheChosenStartPage(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	require.Equal(t, "dashboard", f.auth.startPageOf(f.storageID, f.user.ID), "the column default")

	rec := f.do(http.MethodPatch, f.base()+"/membership", `{"start_page":"inventory"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		StartPage string `json:"start_page"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "inventory", body.StartPage)
	assert.Equal(t, "inventory", f.auth.startPageOf(f.storageID, f.user.ID))
}

// TestPatchMembershipTouchesOnlyTheCallersOwnRow is the write half of the
// isolation claim: two members of one storage, one PATCH, and the other
// member's row untouched. There is no user id in the route to aim it anywhere
// else, and this is what says so.
func TestPatchMembershipTouchesOnlyTheCallersOwnRow(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	other, _ := f.auth.addUser(t, false)
	f.auth.addMember(f.storageID, other.ID)

	require.Equal(t, http.StatusOK, f.do(http.MethodPatch, f.base()+"/membership", `{"start_page":"ingest"}`).Code)

	assert.Equal(t, "ingest", f.auth.startPageOf(f.storageID, f.user.ID))
	assert.Equal(t, "dashboard", f.auth.startPageOf(f.storageID, other.ID),
		"the other member of the same storage keeps their own choice")
}

// TestPatchMembershipRefusesAUserIdInTheBody — the body's contract is one
// field. A client that invents a user id is told its body was refused rather
// than left believing the extra half aimed the write somewhere.
//
// It would not have been honoured either way: the id comes from the session.
// This pins the honesty, not a hole.
func TestPatchMembershipRefusesAUserIdInTheBody(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	other, _ := f.auth.addUser(t, false)
	f.auth.addMember(f.storageID, other.ID)

	rec := f.do(http.MethodPatch, f.base()+"/membership",
		`{"start_page":"inbox","user_id":"`+other.ID.String()+`"}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "validation_failed", errorCode(t, rec))
	assert.Equal(t, "dashboard", f.auth.startPageOf(f.storageID, other.ID))
	assert.Equal(t, "dashboard", f.auth.startPageOf(f.storageID, f.user.ID),
		"a refused body writes nothing at all, not even the caller's own row")
}

// TestPatchMembershipRejectsAValueOutsideTheList — the list is closed, the
// refusal is a 422, and it names the field so a form can mark it.
//
// "DASHBOARD" and "settings" are the two near misses worth naming: a
// case-insensitive comparison and a page that exists but takes no one's day.
func TestPatchMembershipRejectsAValueOutsideTheList(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"settings", "review", "", "DASHBOARD", "admin"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			f := newAPIFixture(t)
			rec := f.do(http.MethodPatch, f.base()+"/membership", `{"start_page":"`+value+`"}`)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			assert.Equal(t, "validation_failed", errorCode(t, rec))
			assert.Contains(t, errorFields(t, rec), "start_page",
				"the message must name the field the form has to mark")
			assert.Equal(t, "dashboard", f.auth.startPageOf(f.storageID, f.user.ID), "nothing was written")
		})
	}
}

// TestPatchMembershipIs404WhenTheRowWentAwayMidRequest — the gate let the
// request in, so the row existed on arrival. A row revoked since is the answer
// a non-member gets, which is the answer an unknown storage gets: never a 500
// about a race nobody can act on, and never a 403.
func TestPatchMembershipIs404WhenTheRowWentAwayMidRequest(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.dropMembershipRow(f.storageID, f.user.ID)

	rec := f.do(http.MethodPatch, f.base()+"/membership", `{"start_page":"products"}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", errorCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "debug_reason", "production never names the reason")
}

// TestMeReturnsOnlyTheCallersOwnStartPage is the read half of the isolation
// claim: two members of one storage holding different values, each reading
// their own and nobody else's.
//
// A join that dropped the user filter would still return one row per storage
// and still look right in a single-member test. That is why this one has two.
func TestMeReturnsOnlyTheCallersOwnStartPage(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	other, otherSession := f.auth.addUser(t, false)
	f.auth.addMember(f.storageID, other.ID)

	require.Equal(t, http.StatusOK,
		f.do(http.MethodPatch, f.base()+"/membership", `{"start_page":"locations"}`).Code)
	require.Equal(t, http.StatusOK,
		f.doKeyedAs(otherSession.ID, http.MethodPatch, f.base()+"/membership", `{"start_page":"shopping_list"}`, "").Code)

	mine := meStorages(t, f.do(http.MethodGet, "/api/auth/me", "").Body)
	require.Len(t, mine, 1)
	assert.Equal(t, f.storageID.String(), mine[0].ID)
	assert.Equal(t, "locations", mine[0].StartPage)

	theirs := meStorages(t, f.doKeyedAs(otherSession.ID, http.MethodGet, "/api/auth/me", "", "").Body)
	require.Len(t, theirs, 1)
	assert.Equal(t, f.storageID.String(), theirs[0].ID, "the same storage")
	assert.Equal(t, "shopping_list", theirs[0].StartPage, "and each member's own value")
}

// TestMeDefaultsToTheDashboard — an untouched membership reads as the column
// default, so no client has to invent one.
func TestMeDefaultsToTheDashboard(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)

	storages := meStorages(t, f.do(http.MethodGet, "/api/auth/me", "").Body)

	require.Len(t, storages, 1)
	assert.Equal(t, "dashboard", storages[0].StartPage)
}

// TestMeStillCarriesNoAdminFlag re-pins the rule that matters most on this
// response now that a field has been added to its storages array. An additive
// field is exactly the edit during which a second one gets added by accident,
// and docs/specs/03-auth-and-multi-tenancy.md's whole design is that no
// shipped client can branch on admin status because no response carries it.
//
// account_test.go's TestPasswordRoutesLeakNothing makes the same scan against
// PATCH /api/auth/me. Both routes render through writeMe, so this is the same
// guarantee checked from the read side, where the new field actually lives.
func TestMeStillCarriesNoAdminFlag(t *testing.T) {
	t.Parallel()

	f := newAPIFixture(t)
	f.auth.setAdmin(f.user.ID, true)
	f.user.IsAdmin = true

	rec := f.do(http.MethodGet, "/api/auth/me", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "is_admin")
}

// TestStartPagesMatchTheShippedNavigationMap keeps the closed list in step
// across the places it is written: the column's CHECK, the Go allowlist, and
// web/static/js/nav.js's value→path map.
//
// Adding a page to one and not the others fails in a way nothing else catches.
// A value the API accepts and the frontend cannot route lands the user on a
// blank page; a value the frontend offers and the API refuses is a select box
// that answers 422 when it is used.
func TestStartPagesMatchTheShippedNavigationMap(t *testing.T) {
	t.Parallel()

	assets, err := web.Static()
	require.NoError(t, err)
	body, err := fs.ReadFile(assets, "js/nav.js")
	require.NoError(t, err, "web/static/js/nav.js must be embedded")

	_, after, found := strings.Cut(string(body), "export const START_PAGES = {")
	require.True(t, found, "nav.js must export START_PAGES as an object literal")
	block, _, found := strings.Cut(after, "}")
	require.True(t, found, "the START_PAGES literal must be closed")

	keyPattern := regexp.MustCompile(`(?m)^\s*(\w+):`)
	fromJS := make([]string, 0, len(httpapi.StartPages()))
	for _, match := range keyPattern.FindAllStringSubmatch(block, -1) {
		fromJS = append(fromJS, match[1])
	}
	require.NotEmpty(t, fromJS, "a literal that parsed to nothing would pass this test while checking nothing")

	fromGo := httpapi.StartPages()
	sort.Strings(fromJS)
	sort.Strings(fromGo)
	assert.Equal(t, fromGo, fromJS,
		"internal/httpapi/membership.go and web/static/js/nav.js must name the same pages")
}
