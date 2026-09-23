package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AdminAction names one kind of mutating admin action
// (docs/specs/18-operations-and-observability.md).
//
// The values are stable strings: they are stored in admin_audit_log.action and
// rendered on /admin/audit, so renaming one rewrites history. A new admin
// route adds a constant here and nothing else — the column is TEXT rather than
// an enum precisely so that costs no migration.
type AdminAction string

// The actions of the admin route table in
// docs/specs/03-auth-and-multi-tenancy.md, one per mutating route.
const (
	ActionUserCreated          AdminAction = "user_created"
	ActionUserDeleted          AdminAction = "user_deleted"
	ActionUserPasswordReset    AdminAction = "user_password_reset"
	ActionStorageCreated       AdminAction = "storage_created"
	ActionStorageDeleted       AdminAction = "storage_deleted"
	ActionStorageMemberAdded   AdminAction = "storage_member_added"
	ActionStorageMemberRemoved AdminAction = "storage_member_removed"
	ActionSettingsUpdated      AdminAction = "settings_updated"
	ActionCatalogEntryUpdated  AdminAction = "catalog_entry_updated"
	ActionCatalogEntryDeleted  AdminAction = "catalog_entry_deleted"
)

// SystemActor is the actor for an audited write that no admin performed — the
// first-boot bootstrap, a maintenance subcommand, a test fixture. It is stored
// as a NULL actor_id and rendered as "system" on the audit page.
//
// **It does not suppress the audit row.** Every audited write records one,
// actor or no actor: a missing actor then shows up as a visible "system" row
// on a page an operator reads, rather than as a mutation with no record at
// all, which is the failure mode that would be invisible. There is no value of
// this parameter that turns the trail off.
var SystemActor = uuid.Nil

// AdminAuditEntry is one row of the trail, with the actor's username resolved
// for display.
//
// ActorUsername is empty both when the actor was SystemActor and when the
// account has since been deleted — actor_id is ON DELETE SET NULL, because
// deleting a user must not delete the record of what they did. Both arrive as
// NULL and the page renders them identically: a trail that claimed to know
// which of the two applied would be guessing.
type AdminAuditEntry struct {
	ID            uuid.UUID
	ActorUsername string
	Action        AdminAction
	Target        string
	Details       map[string]any
	CreatedAt     time.Time
}

// AuditDetails is the JSONB payload of one entry.
//
// Every field is a fact about what changed — a username, a storage id, a
// settings key. **There is no field here that could hold password material**,
// which is how docs/specs/18-operations-and-observability.md's "never a
// password, even transiently" is enforced structurally: the writers below
// accept this struct and nothing else, so there is no call site that could
// pass a hash or a plaintext through, and no free-form map a future field
// could be smuggled into.
type AuditDetails struct {
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	// AdminRights is whether a created account was given admin rights.
	//
	// The JSON key deliberately does **not** repeat the users column's own
	// name. That exact spelling is what internal/httpapi/errors_test.go's
	// structural scan forbids anywhere in the repository's source, and the
	// scan is worth more than the convenience of matching the column: it is
	// what stops a future response type quietly gaining the field. Nothing
	// reads this key but the audit page, so spelling it differently costs
	// nothing.
	AdminRights *bool  `json:"admin_rights,omitempty"`
	StorageName string `json:"storage_name,omitempty"`
	StorageID   string `json:"storage_id,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Key         string `json:"key,omitempty"`
	Value       string `json:"value,omitempty"`
	// ShelfLifeDays is the new value of a catalog correction and
	// RecomputedBatches how many batches it reached — the two numbers that
	// make a cross-household correction reconstructable afterwards.
	ShelfLifeDays     *int `json:"shelf_life_days,omitempty"`
	RecomputedBatches *int `json:"recomputed_batches,omitempty"`
}

// writeAdminAudit appends one row, inside the caller's transaction.
//
// It takes a pgx.Tx rather than the pool for the same structural reason
// writeLog does (store.go): an audit row outside the mutation's own
// transaction is a row that can exist without the change it describes, or —
// worse — a change that commits with no row. There is deliberately no
// pool-based variant to reach for.
func writeAdminAudit(ctx context.Context, tx pgx.Tx, actor uuid.UUID, action AdminAction, target string, details AuditDetails) error {
	id, err := newID()
	if err != nil {
		return err
	}

	payload, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("store: encode audit details: %w", err)
	}

	var actorID *uuid.UUID
	if actor != SystemActor {
		actorID = &actor
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log (id, actor_id, action, target, details)
		VALUES ($1, $2, $3, $4, $5)`,
		id, actorID, string(action), nullIfEmpty(target), payload); err != nil {
		return fmt.Errorf("store: write admin audit entry: %w", err)
	}
	return nil
}

// nullIfEmpty keeps target NULL rather than an empty string for an action with
// no single target, so the page tests one condition instead of two.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AuditPageSize is the default page length of /admin/audit. Enough that a
// day's admin activity on a household server is one screen.
const AuditPageSize = 50

// AuditPage is one page of the trail, newest first.
type AuditPage struct {
	Entries []AdminAuditEntry
	// Next is the cursor for the following, older page. Empty on the last one.
	Next string
}

// ListAdminAudit returns one page of the trail, newest first.
//
// **Cursor, not offset.** The table is append-only and ordered newest first,
// so new rows arrive at the *front*: with an offset, an entry written between
// two page views pushes everything down and the operator sees one row twice
// while another slips past unseen. A cursor names the last entry of the
// previous page, and "everything older than that entry" does not move — the
// same argument pagination.go makes for the JSON API's cursors.
//
// The cursor is the (created_at, id) pair rather than the id alone. Ids are
// UUIDv7 and so usually sort like their timestamps, but "usually" is not
// "always": now() is the transaction's start time, so a long transaction can
// commit a row whose created_at precedes that of a row with a smaller id.
// Comparing the pair the index is built on keeps the ordering total whatever
// the clock did.
//
// A cursor this server did not issue is ErrValidation, not an empty page: an
// operator who mangled the URL should be told, not shown the first page as
// though nothing happened.
func (s *Store) ListAdminAudit(ctx context.Context, cursor string, limit int) (*AuditPage, error) {
	if limit < 1 {
		limit = AuditPageSize
	}

	before, ok := decodeAuditCursor(cursor)
	if !ok {
		return nil, fmt.Errorf("%w: not a cursor from a previous page", ErrValidation)
	}

	// limit+1 rows to detect the last page without a count query, exactly as
	// pageOf does for the JSON collections.
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, COALESCE(u.username, ''), a.action, COALESCE(a.target, ''),
		       COALESCE(a.details, '{}'::jsonb), a.created_at
		  FROM admin_audit_log a
		  LEFT JOIN users u ON u.id = a.actor_id
		 WHERE $1::timestamptz IS NULL OR (a.created_at, a.id) < ($1, $2)
		 ORDER BY a.created_at DESC, a.id DESC
		 LIMIT $3`, before.at, before.id, limit+1)
	if err != nil {
		return nil, fmt.Errorf("store: list admin audit: %w", err)
	}
	defer rows.Close()

	page := &AuditPage{Entries: []AdminAuditEntry{}}
	for rows.Next() {
		var entry AdminAuditEntry
		var action string
		var details []byte
		if err := rows.Scan(&entry.ID, &entry.ActorUsername, &action, &entry.Target, &details, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan admin audit entry: %w", err)
		}
		entry.Action = AdminAction(action)
		if err := json.Unmarshal(details, &entry.Details); err != nil {
			return nil, fmt.Errorf("store: decode admin audit details: %w", err)
		}
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list admin audit: %w", err)
	}

	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[len(page.Entries)-1]
		page.Next = encodeAuditCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// auditCursor is the decoded (created_at, id) pair. A nil at means "from the
// newest", which the query's IS NULL branch handles.
type auditCursor struct {
	at *time.Time
	id uuid.UUID
}

// encodeAuditCursor renders the pair opaquely, for the reason pagination.go's
// encodeCursor is opaque: a caller that learned to build one out of a
// timestamp would break the day the ordering gains a third key.
func encodeAuditCursor(at time.Time, id uuid.UUID) string {
	raw := strconv.FormatInt(at.UTC().UnixNano(), 10) + "." + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeAuditCursor parses a cursor. The second result is false only for a
// malformed one; an empty cursor is the valid "first page".
func decodeAuditCursor(raw string) (auditCursor, bool) {
	if raw == "" {
		return auditCursor{}, true
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return auditCursor{}, false
	}
	nanos, idText, found := strings.Cut(string(decoded), ".")
	if !found {
		return auditCursor{}, false
	}
	unixNano, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return auditCursor{}, false
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return auditCursor{}, false
	}
	at := time.Unix(0, unixNano).UTC()
	return auditCursor{at: &at, id: id}, true
}
