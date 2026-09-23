package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

// auditRowsFor returns the trail entries targeting one id, newest first.
//
// Read with plain SQL rather than through ListAdminAudit, for the reason the
// rest of this package's tests set up and assert with plain SQL: an assertion
// made through the reader under test would hide a reader that filtered away
// the row it should have found.
func auditRowsFor(t *testing.T, ctx context.Context, target string) []struct {
	Action   string
	ActorID  *uuid.UUID
	Details  string
	TargetID *string
} {
	t.Helper()

	rows, err := testPool.Query(ctx, `
		SELECT action, actor_id, details::text, target
		  FROM admin_audit_log
		 WHERE target = $1
		 ORDER BY created_at DESC, id DESC`, target)
	require.NoError(t, err)
	defer rows.Close()

	var out []struct {
		Action   string
		ActorID  *uuid.UUID
		Details  string
		TargetID *string
	}
	for rows.Next() {
		var row struct {
			Action   string
			ActorID  *uuid.UUID
			Details  string
			TargetID *string
		}
		require.NoError(t, rows.Scan(&row.Action, &row.ActorID, &row.Details, &row.TargetID))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestEveryMutatingAdminActionWritesExactlyOneAuditRow is the acceptance
// criterion of docs/specs/18-operations-and-observability.md, taken literally:
// one row, not "at least one".
//
// The count matters as much as the existence. A helper that wrote a row and a
// caller that also wrote one would produce a trail that double-counts every
// action, and an "is there a row" assertion would be perfectly happy with it.
func TestEveryMutatingAdminActionWritesExactlyOneAuditRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	actor := newUser(t, ctx)

	t.Run("user created", func(t *testing.T) {
		user, err := s.AdminCreateUser(ctx, actor, store.NewUser{
			Username: "created-" + uuid.NewString(), PasswordHash: "$argon2id$hash", DisplayName: "Created",
		})
		require.NoError(t, err)

		rows := auditRowsFor(t, ctx, user.ID.String())
		require.Len(t, rows, 1)
		assert.Equal(t, string(store.ActionUserCreated), rows[0].Action)
		require.NotNil(t, rows[0].ActorID)
		assert.Equal(t, actor, *rows[0].ActorID)
		assert.Contains(t, rows[0].Details, user.Username)
	})

	t.Run("user password reset", func(t *testing.T) {
		target := newUser(t, ctx)
		require.NoError(t, s.AdminResetPassword(ctx, actor, target, "$argon2id$fresh"))

		rows := auditRowsFor(t, ctx, target.String())
		require.Len(t, rows, 1)
		assert.Equal(t, string(store.ActionUserPasswordReset), rows[0].Action)
	})

	t.Run("user deleted", func(t *testing.T) {
		target := newUser(t, ctx)
		require.NoError(t, s.DeleteUser(ctx, actor, target))

		rows := auditRowsFor(t, ctx, target.String())
		require.Len(t, rows, 1)
		assert.Equal(t, string(store.ActionUserDeleted), rows[0].Action)
	})

	t.Run("storage created and deleted", func(t *testing.T) {
		storage, err := s.CreateStorage(ctx, actor, "Audited Pantry")
		require.NoError(t, err)

		rows := auditRowsFor(t, ctx, storage.ID.String())
		require.Len(t, rows, 1)
		assert.Equal(t, string(store.ActionStorageCreated), rows[0].Action)
		assert.Contains(t, rows[0].Details, "Audited Pantry")

		require.NoError(t, s.DeleteStorage(ctx, actor, storage.ID))
		rows = auditRowsFor(t, ctx, storage.ID.String())
		require.Len(t, rows, 2)
		assert.Equal(t, string(store.ActionStorageDeleted), rows[0].Action)
	})

	t.Run("membership granted and revoked", func(t *testing.T) {
		storage, err := s.CreateStorage(ctx, actor, "Membership Household")
		require.NoError(t, err)
		member := newUser(t, ctx)

		require.NoError(t, s.AddMember(ctx, actor, storage.ID, member))
		require.NoError(t, s.RemoveMember(ctx, actor, storage.ID, member))

		rows := auditRowsFor(t, ctx, storage.ID.String())
		require.Len(t, rows, 3, "create, grant, revoke")
		assert.Equal(t, string(store.ActionStorageMemberRemoved), rows[0].Action)
		assert.Equal(t, string(store.ActionStorageMemberAdded), rows[1].Action)
		assert.Contains(t, rows[1].Details, member.String())
	})

	t.Run("settings updated", func(t *testing.T) {
		key := "audited_setting_" + uuid.NewString()[:8]
		require.NoError(t, s.SetSetting(ctx, key, "gemini-2.5-flash", actor))

		rows := auditRowsFor(t, ctx, key)
		require.Len(t, rows, 1)
		assert.Equal(t, string(store.ActionSettingsUpdated), rows[0].Action)
		assert.Contains(t, rows[0].Details, "gemini-2.5-flash")
	})

	t.Run("catalog entry corrected and deleted", func(t *testing.T) {
		entry, err := s.InsertCatalogProduct(ctx, store.NewCatalogProduct{
			DisplayName: "Audited Oat Milk " + uuid.NewString()[:8],
		})
		require.NoError(t, err)

		_, err = s.CorrectCatalogShelfLife(ctx, actor, entry.ID, ptrInt(120))
		require.NoError(t, err)
		require.NoError(t, s.DeleteCatalogProduct(ctx, actor, entry.ID))

		rows := auditRowsFor(t, ctx, entry.ID.String())
		require.Len(t, rows, 2)
		assert.Equal(t, string(store.ActionCatalogEntryDeleted), rows[0].Action)
		assert.Equal(t, string(store.ActionCatalogEntryUpdated), rows[1].Action)
		assert.Contains(t, rows[1].Details, "120")
		assert.Contains(t, rows[1].Details, "recomputed_batches")
	})
}

// TestAFailedAdminMutationWritesNoAuditRow is the half that proves the writes
// really share a transaction.
//
// Without it, "the audit row is written in the same transaction" is an
// unverified claim: an implementation that wrote the row first and then
// attempted the mutation would pass every "exactly one row" assertion above
// while filling the trail with actions that never happened.
func TestAFailedAdminMutationWritesNoAuditRow(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	actor := newUser(t, ctx)

	t.Run("duplicate username", func(t *testing.T) {
		username := "clash-" + uuid.NewString()
		first, err := s.AdminCreateUser(ctx, actor, store.NewUser{
			Username: username, PasswordHash: "$argon2id$hash", DisplayName: "First",
		})
		require.NoError(t, err)

		before := countRows(t, ctx, `SELECT count(*) FROM admin_audit_log`)
		_, err = s.AdminCreateUser(ctx, actor, store.NewUser{
			Username: username, PasswordHash: "$argon2id$hash", DisplayName: "Second",
		})
		require.ErrorIs(t, err, store.ErrDuplicate)

		assert.Equal(t, before, countRows(t, ctx, `SELECT count(*) FROM admin_audit_log`),
			"a rejected create must leave no trace in the trail")
		assert.Len(t, auditRowsFor(t, ctx, first.ID.String()), 1,
			"and must not add a second row against the row that does exist")
	})

	t.Run("deleting a user that does not exist", func(t *testing.T) {
		missing := newUUID(t)
		before := countRows(t, ctx, `SELECT count(*) FROM admin_audit_log`)

		require.ErrorIs(t, s.DeleteUser(ctx, actor, missing), store.ErrNotFound)

		assert.Equal(t, before, countRows(t, ctx, `SELECT count(*) FROM admin_audit_log`))
		assert.Empty(t, auditRowsFor(t, ctx, missing.String()))
	})

	t.Run("granting membership on a storage that does not exist", func(t *testing.T) {
		missing := newUUID(t)
		before := countRows(t, ctx, `SELECT count(*) FROM admin_audit_log`)

		require.ErrorIs(t, s.AddMember(ctx, actor, missing, newUser(t, ctx)), store.ErrNotFound)

		assert.Equal(t, before, countRows(t, ctx, `SELECT count(*) FROM admin_audit_log`))
	})
}

// TestAuditDetailsNeverCarryPasswordMaterial is the spec's own assertion,
// named for the two actions that handle a password at all.
//
// It looks for the plaintext, the hash, and the argon2 marker, in the details
// and in every other column: the point is not that the current code omits
// them, but that nothing anywhere in the row does.
func TestAuditDetailsNeverCarryPasswordMaterial(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	actor := newUser(t, ctx)

	const (
		plaintext = "zzz-never-in-the-trail-zzz"
		hash      = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$ZGlnZXN0"
	)

	created, err := s.AdminCreateUser(ctx, actor, store.NewUser{
		Username: "pw-" + uuid.NewString(), PasswordHash: hash, DisplayName: "Created",
	})
	require.NoError(t, err)
	require.NoError(t, s.AdminResetPassword(ctx, actor, created.ID, hash))

	rows, err := testPool.Query(ctx, `
		SELECT coalesce(action,'') || ' ' || coalesce(target,'') || ' ' || coalesce(details::text,'')
		  FROM admin_audit_log WHERE target = $1`, created.ID.String())
	require.NoError(t, err)
	defer rows.Close()

	found := 0
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		found++
		assert.NotContains(t, row, plaintext, "no plaintext password")
		assert.NotContains(t, row, hash, "no password hash")
		assert.NotContains(t, row, "argon2", "not even the hash's algorithm marker")
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 2, found, "the create and the reset both recorded something to inspect")
}

// TestAuditTrailPagesNewestFirstWithoutRepeatingOrSkipping exercises the
// cursor over more entries than one page holds.
//
// The assertion that matters is the set: every entry appears exactly once
// across the pages. An off-by-one in the cursor comparison — `<=` where `<`
// belongs, or the reverse — shows up as a duplicate or a hole, and both are
// invisible to a test that only checks the first page's ordering.
func TestAuditTrailPagesNewestFirstWithoutRepeatingOrSkipping(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	actor := newUser(t, ctx)

	// A target nothing else in this package writes, so the page assertions are
	// about these rows and not about whatever else the suite recorded.
	const total = 7
	var want []string
	for i := 0; i < total; i++ {
		storage, err := s.CreateStorage(ctx, actor, "Paged Household")
		require.NoError(t, err)
		want = append(want, storage.ID.String())
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < total+2; page++ {
		got, err := s.ListAdminAudit(ctx, cursor, 2)
		require.NoError(t, err)

		for i := 1; i < len(got.Entries); i++ {
			assert.False(t, got.Entries[i].CreatedAt.After(got.Entries[i-1].CreatedAt),
				"entries are newest first")
		}
		for _, entry := range got.Entries {
			seen[entry.Target]++
		}
		if got.Next == "" {
			break
		}
		cursor = got.Next
	}

	for _, target := range want {
		assert.Equal(t, 1, seen[target], "entry for %s appears exactly once across pages", target)
	}
}

// TestAuditTrailResolvesTheActorAndSurvivesTheirDeletion pins the
// ON DELETE SET NULL in migration 00010.
//
// Deleting an admin must not delete the record of what they did — which is
// most of the point of keeping the trail at all, since "the admin who did this
// is gone" is exactly the situation somebody reads it in.
func TestAuditTrailResolvesTheActorAndSurvivesTheirDeletion(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	actor, err := s.AdminCreateUser(ctx, store.SystemActor, store.NewUser{
		Username: "doomed-" + uuid.NewString(), PasswordHash: "$argon2id$hash", DisplayName: "Doomed",
	})
	require.NoError(t, err)

	storage, err := s.CreateStorage(ctx, actor.ID, "Outlives Its Author")
	require.NoError(t, err)

	page, err := s.ListAdminAudit(ctx, "", store.AuditPageSize)
	require.NoError(t, err)
	require.NotEmpty(t, page.Entries)
	require.Equal(t, storage.ID.String(), page.Entries[0].Target)
	assert.Equal(t, actor.Username, page.Entries[0].ActorUsername, "the actor is resolved for display")

	require.NoError(t, s.DeleteUser(ctx, store.SystemActor, actor.ID))

	rows := auditRowsFor(t, ctx, storage.ID.String())
	require.Len(t, rows, 1, "the entry survives its actor")
	assert.Nil(t, rows[0].ActorID, "and its actor_id is nulled rather than the row removed")
}

// TestSystemActorRecordsAnEntryWithNoActor pins the other half of
// store.SystemActor's contract: it means "nobody performed this", not "do not
// record this". A value that silently suppressed the row would turn a
// forgotten actor into a missing audit entry, which is the failure this design
// exists to avoid.
func TestSystemActorRecordsAnEntryWithNoActor(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()

	storage, err := s.CreateStorage(ctx, store.SystemActor, "Unattributed Household")
	require.NoError(t, err)

	rows := auditRowsFor(t, ctx, storage.ID.String())
	require.Len(t, rows, 1, "the row is written even with no actor")
	assert.Nil(t, rows[0].ActorID)
}

// TestAuditCursorFromNowhereIsRejected: a mangled cursor is a validation
// error, not silently the first page. Showing page one to somebody who asked
// for page four would look like the trail had been truncated.
func TestAuditCursorFromNowhereIsRejected(t *testing.T) {
	s := requireDB(t)

	_, err := s.ListAdminAudit(context.Background(), "not-a-cursor", store.AuditPageSize)
	assert.ErrorIs(t, err, store.ErrValidation)
}
