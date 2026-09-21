package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/store"
)

// TestEveryMutatingAdminRouteIsAudited walks the mutating half of the admin
// route table in docs/specs/03-auth-and-multi-tenancy.md and asserts that each
// one leaves exactly one entry, attributed to the caller.
//
// The store package proves the row and the mutation share a transaction
// (internal/store/audit_test.go). What is proved here is the half that lives
// at this layer and that no database test can see: that the *route* reaches an
// audited store method at all, and that the actor it records is the user the
// gates resolved rather than anything from the request.
//
// A route added to the admin group without an audit write is the failure this
// catches — and it is a silent one, because such a route works perfectly.
func TestEveryMutatingAdminRouteIsAudited(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	storage, err := f.auth.CreateStorage(context.Background(), store.SystemActor, "Audited Household")
	require.NoError(t, err)
	member, _ := f.auth.addUser(t, false)

	entry := uuid.New()
	f.auth.catalog = []store.CatalogProduct{
		{ID: entry, DisplayName: "Audited Oat Milk", ItemType: store.ItemPerishable},
	}

	// The storage's own creation was seeded with store.SystemActor above, so
	// the entries counted below are only the ones the routes wrote.
	require.Len(t, f.auth.auditEntries(), 1)

	// Each step is one request; want is the action it must record. They run in
	// order against one fixture because several depend on the previous one's
	// effect (a member must be added before it can be removed).
	steps := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		want   store.AdminAction
	}{
		{
			name: "create user", method: http.MethodPost, path: "/api/admin/users",
			body:   `{"username":"audited-user","password":"a long enough one","is_admin":false}`,
			status: http.StatusCreated, want: store.ActionUserCreated,
		},
		{
			name: "reset a password", method: http.MethodPost,
			path:   "/api/admin/users/" + member.ID.String() + "/password",
			body:   `{"new_password":"another long one"}`,
			status: http.StatusNoContent, want: store.ActionUserPasswordReset,
		},
		{
			name: "create storage", method: http.MethodPost, path: "/api/admin/storages",
			body: `{"name":"Second Household"}`, status: http.StatusCreated,
			want: store.ActionStorageCreated,
		},
		{
			name: "grant membership", method: http.MethodPost,
			path:   "/api/admin/storages/" + storage.ID.String() + "/members",
			body:   `{"user_id":"` + member.ID.String() + `"}`,
			status: http.StatusNoContent, want: store.ActionStorageMemberAdded,
		},
		{
			name: "revoke membership", method: http.MethodDelete,
			path:   "/api/admin/storages/" + storage.ID.String() + "/members/" + member.ID.String(),
			status: http.StatusNoContent, want: store.ActionStorageMemberRemoved,
		},
		{
			name: "update settings", method: http.MethodPut, path: "/api/admin/settings",
			body: `{"gemini_model":"gemini-2.5-flash"}`, status: http.StatusOK,
			want: store.ActionSettingsUpdated,
		},
		{
			name: "correct a catalog entry", method: http.MethodPatch,
			path: "/api/admin/catalog/" + entry.String(),
			body: `{"default_shelf_life_days":90}`, status: http.StatusOK,
			want: store.ActionCatalogEntryUpdated,
		},
		{
			name: "delete a catalog entry", method: http.MethodDelete,
			path: "/api/admin/catalog/" + entry.String(), status: http.StatusNoContent,
			want: store.ActionCatalogEntryDeleted,
		},
		{
			name: "delete storage", method: http.MethodDelete,
			path: "/api/admin/storages/" + storage.ID.String(), status: http.StatusNoContent,
			want: store.ActionStorageDeleted,
		},
		{
			name: "delete user", method: http.MethodDelete,
			path: "/api/admin/users/" + member.ID.String(), status: http.StatusNoContent,
			want: store.ActionUserDeleted,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			before := len(f.auth.auditEntries())

			rec := f.do(step.method, step.path, step.body)
			require.Equal(t, step.status, rec.Code, "body: %s", rec.Body.String())

			after := f.auth.auditEntries()
			require.Len(t, after, before+1, "exactly one entry, not none and not two")

			latest := after[len(after)-1]
			assert.Equal(t, step.want, latest.Action)
			assert.Equal(t, f.user.Username, latest.ActorUsername,
				"attributed to the caller the gates resolved")
		})
	}
}

// TestARefusedAdminRouteRecordsNothing.
//
// The trail has to be a record of what happened. A 404 for a user that does
// not exist, or a 409 refusing an admin's attempt to delete themselves, is not
// an action and must leave no entry — otherwise reading the trail after an
// incident means guessing which lines describe changes and which describe
// attempts.
func TestARefusedAdminRouteRecordsNothing(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	before := len(f.auth.auditEntries())

	refusals := []struct {
		name           string
		method, path   string
		body           string
		expectedStatus int
	}{
		{
			name: "deleting a user that does not exist", method: http.MethodDelete,
			path: "/api/admin/users/" + uuid.NewString(), expectedStatus: http.StatusNotFound,
		},
		{
			name: "deleting your own account", method: http.MethodDelete,
			path: "/api/admin/users/" + f.user.ID.String(), expectedStatus: http.StatusConflict,
		},
		{
			name: "creating a user with too short a password", method: http.MethodPost,
			path: "/api/admin/users", body: `{"username":"short","password":"abc"}`,
			expectedStatus: http.StatusUnprocessableEntity,
		},
		{
			name: "granting membership on a storage that does not exist", method: http.MethodPost,
			path: "/api/admin/storages/" + uuid.NewString() + "/members",
			body: `{"user_id":"` + uuid.NewString() + `"}`, expectedStatus: http.StatusNotFound,
		},
	}

	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(tc.method, tc.path, tc.body)
			require.Equal(t, tc.expectedStatus, rec.Code, "body: %s", rec.Body.String())
			assert.Len(t, f.auth.auditEntries(), before, "a refusal is not an action")
		})
	}
}

// TestAuditEntriesFromTheRoutesCarryNoPasswordMaterial is the spec's
// "admin_audit_log.details never contains password material — asserted for
// user-create and password-reset actions", checked at the layer where the
// password is actually in hand.
func TestAuditEntriesFromTheRoutesCarryNoPasswordMaterial(t *testing.T) {
	t.Parallel()

	const password = "zzz-admin-typed-this-zzz"

	f := newAdminFixture(t)
	victim, _ := f.auth.addUser(t, false)

	require.Equal(t, http.StatusCreated,
		f.do(http.MethodPost, "/api/admin/users",
			`{"username":"fresh-account","password":"`+password+`"}`).Code)
	require.Equal(t, http.StatusNoContent,
		f.do(http.MethodPost, "/api/admin/users/"+victim.ID.String()+"/password",
			`{"new_password":"`+password+`"}`).Code)

	entries := f.auth.auditEntries()
	require.Len(t, entries, 2)

	for _, entry := range entries {
		for key, value := range entry.Details {
			rendered, ok := value.(string)
			if !ok {
				continue
			}
			assert.NotContainsf(t, rendered, password,
				"details[%q] of %s carries the password", key, entry.Action)
			assert.NotContainsf(t, rendered, "argon2",
				"details[%q] of %s carries a password hash", key, entry.Action)
		}
	}
}

// TestTheAuditPageRendersTheTrail: the page an operator actually reads.
//
// Asserting on the rendered HTML rather than on a JSON shape is the point —
// there is no JSON counterpart, by design, so this markup is the entire
// surface the trail has.
func TestTheAuditPageRendersTheTrail(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	require.Equal(t, http.StatusCreated,
		f.do(http.MethodPost, "/api/admin/storages", `{"name":"Rendered Household"}`).Code)

	rec := f.do(http.MethodGet, "/admin/audit", "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, string(store.ActionStorageCreated))
	assert.Contains(t, body, "Rendered Household", "the details are rendered, not just the action")
	assert.Contains(t, body, f.user.Username, "and so is who did it")

	// The same headers the /admin page sets. A second admin page that rendered
	// without the CSP would look identical in a browser and be a hole.
	assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.Contains(t, csp, "nonce-", "the style block runs by nonce or not at all")
}

// TestTheAuditPageIsEmptyWithoutBlowingUp — a fresh deployment has no entries,
// and the first thing a curious operator does is open the page.
func TestTheAuditPageIsEmptyWithoutBlowingUp(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	rec := f.do(http.MethodGet, "/admin/audit", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Nothing recorded yet")
}

// TestTheAuditPagePagesToOlderEntries covers the "older" link, which is the
// only way past the first page.
func TestTheAuditPagePagesToOlderEntries(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for i := 0; i < store.AuditPageSize+3; i++ {
		require.Equal(t, http.StatusCreated,
			f.do(http.MethodPost, "/api/admin/storages", `{"name":"Household"}`).Code)
	}

	first := f.do(http.MethodGet, "/admin/audit", "")
	require.Equal(t, http.StatusOK, first.Code)

	cursor := cursorFromPage(t, first.Body.String())
	require.NotEmpty(t, cursor, "a full page offers a link to the older one")

	older := f.do(http.MethodGet, "/admin/audit?cursor="+cursor, "")
	require.Equal(t, http.StatusOK, older.Code)
	assert.NotEqual(t, first.Body.String(), older.Body.String(),
		"the older page is a different page, not the first one again")
}

// cursorFromPage pulls the cursor out of the rendered "Older" link.
func cursorFromPage(t *testing.T, body string) string {
	t.Helper()

	const marker = `href="/admin/audit?cursor=`
	index := strings.Index(body, marker)
	if index < 0 {
		return ""
	}
	rest := body[index+len(marker):]
	end := strings.IndexByte(rest, '"')
	require.GreaterOrEqual(t, end, 0)
	return rest[:end]
}

// TestTheAuditTrailHasNoJSONRoute.
//
// docs/specs/18-operations-and-observability.md says the trail "is never
// exposed through any non-admin route", and the design answer is that it has
// no JSON route at all. The obvious address for one is asserted absent, so
// that adding it is a deliberate act rather than something that slips in
// beside the other /api/admin routes.
func TestTheAuditTrailHasNoJSONRoute(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	for _, path := range []string{"/api/admin/audit", "/api/admin/audit-log"} {
		rec := f.do(http.MethodGet, path, "")
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s must not exist", path)
	}
}

// TestADeviceSessionCannotReadTheTrail.
//
// A paired client is never an admin client (docs/specs/12-client-api-contract.md),
// whatever its user's is_admin says — and an admin who pairs a phone must not
// thereby hand that phone the record of every admin action on the deployment.
func TestADeviceSessionCannotReadTheTrail(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	device := f.auth.addDeviceSession(f.user.ID, "Kitchen phone")

	rec := sendAs(f.router, device.ID, http.MethodGet, "/admin/audit", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"),
		"refused with the API's 404, not a page")
}

// TestTheAuditPageRejectsACursorItDidNotIssue — showing page one to somebody
// who asked for page four would look like the trail had been truncated, which
// is the opposite of what a trail is for.
func TestTheAuditPageRejectsACursorItDidNotIssue(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t, func(d *httpapi.Deps) { d.Logger = discardLogger() })

	rec := f.do(http.MethodGet, "/admin/audit?cursor=not-a-real-cursor", "")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}
