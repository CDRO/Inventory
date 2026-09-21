package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueViolation is PostgreSQL's SQLSTATE for a duplicate key.
const uniqueViolation = "23505"

// foreignKeyViolation is PostgreSQL's SQLSTATE for a reference to a missing row.
const foreignKeyViolation = "23503"

// ErrDuplicate means a uniquely-constrained value already exists.
var ErrDuplicate = errors.New("store: already exists")

// User is an account.
//
// IsAdmin is present here because the server needs it; it must never reach a
// client. No API response carries this field, and no client-side branch
// depends on it — the admin area is server-rendered precisely so there is
// nothing in the shipped frontend to unlock (docs/specs/03-auth-and-multi-tenancy.md).
type User struct {
	ID       uuid.UUID
	Username string
	// PasswordHash and IsAdmin carry json:"-" as defence in depth.
	//
	// Without a tag, Go's encoder emits an exported field under its own name:
	// json.Encode of a *User — the obvious shortcut for a first cut of
	// GET /api/auth/me, working from the value already in the request context —
	// would put "IsAdmin":true and the argon2 hash straight into the response.
	// Handlers are expected to serialize a DTO instead, but "expected to" is
	// what this project keeps learning not to rely on, and the cost of the tag
	// is nothing.
	PasswordHash string `json:"-"`
	DisplayName  string
	IsAdmin      bool `json:"-"`
	CreatedAt    time.Time
}

// NewUser is the input to CreateUser. There is no public registration: every
// row is created by an admin through the admin view, or by the first-boot
// bootstrap.
type NewUser struct {
	Username     string
	PasswordHash string
	DisplayName  string
	IsAdmin      bool
}

// CreateUser inserts an account, returning ErrDuplicate when the username is
// taken.
//
// **Unaudited, and reachable from exactly one place: the first-boot
// bootstrap** (internal/auth/bootstrap.go), which is not an admin action —
// nobody performed it and there is no session behind it. Account creation *by
// an admin* goes through AdminCreateUser below, which writes the
// admin_audit_log row in the same transaction; the admin handler's store
// interface (internal/httpapi/admin.go) names only that one, so there is no
// path from an admin route to this method.
func (s *Store) CreateUser(ctx context.Context, in NewUser) (*User, error) {
	return insertUser(ctx, s.pool, in)
}

// AdminCreateUser is CreateUser as an audited admin action: the insert and the
// admin_audit_log row commit together or not at all
// (docs/specs/18-operations-and-observability.md).
//
// The audited details are the username, display name and admin flag — what was
// created. **The password hash is not among them**, and cannot be: AuditDetails
// has no field it would fit in (internal/store/audit.go).
func (s *Store) AdminCreateUser(ctx context.Context, actor uuid.UUID, in NewUser) (*User, error) {
	var out *User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		user, err := insertUser(ctx, tx, in)
		if err != nil {
			return err
		}
		out = user
		isAdmin := in.IsAdmin
		return writeAdminAudit(ctx, tx, actor, ActionUserCreated, user.ID.String(), AuditDetails{
			Username:    user.Username,
			DisplayName: user.DisplayName,
			AdminRights: &isAdmin,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertUser is the statement both of the above run, so the audited and
// unaudited paths cannot drift on what a user row is.
func insertUser(ctx context.Context, q querier, in NewUser) (*User, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	row := q.QueryRow(ctx, `
		INSERT INTO users (id, username, password_hash, display_name, is_admin)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, username, password_hash, display_name, is_admin, created_at`,
		id, in.Username, in.PasswordHash, in.DisplayName, in.IsAdmin)

	user, err := scanUser(row)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: username %q is taken", ErrDuplicate, in.Username)
	}
	return user, err
}

// UserByUsername loads an account for a login attempt.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		SELECT id, username, password_hash, display_name, is_admin, created_at
		  FROM users WHERE username = $1`, username))
}

// UserByID loads an account by id.
func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		SELECT id, username, password_hash, display_name, is_admin, created_at
		  FROM users WHERE id = $1`, id))
}

// IsAdmin reports whether a user currently holds admin rights.
//
// It queries the database on every call, by construction: there is no cached
// value, no field on a session row, and no parameter a caller could pass
// instead. That is the point. An admin flag read from anywhere but the
// database at the moment of the request means revoking someone's admin rights
// does not take effect until their session expires — and if it were ever read
// from something the client supplies, it would not mean anything at all
// (docs/specs/03-auth-and-multi-tenancy.md).
//
// This is the data-layer half. The other half — that RequireAdmin calls this
// on every single admin-gated request, and that is_admin never appears in any
// JSON response — belongs to the middleware and the error envelope in
// docs/specs/04-backend-api-conventions.md.
func (s *Store) IsAdmin(ctx context.Context, userID uuid.UUID) (bool, error) {
	var isAdmin bool
	err := s.pool.QueryRow(ctx, `SELECT is_admin FROM users WHERE id = $1`, userID).Scan(&isAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		// A deleted user is not an admin. Reporting an error here would let a
		// race between deletion and a request surface as a 500 instead of the
		// refusal it should be.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read is_admin: %w", err)
	}
	return isAdmin, nil
}

// SetAdmin grants or revokes admin rights.
func (s *Store) SetAdmin(ctx context.Context, userID uuid.UUID, isAdmin bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET is_admin = $1 WHERE id = $2`, isAdmin, userID)
	if err != nil {
		return fmt.Errorf("store: set is_admin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes an account and records who did it.
//
// Their sessions go with it, which is what makes the revocation immediate:
// sessions.user_id is ON DELETE CASCADE, so every credential the person held
// stops working in the same transaction that removes them. A deletion that
// left live sessions behind would be a deletion in name only until those
// sessions happened to expire.
//
// The audit row is written in that same transaction
// (docs/specs/18-operations-and-observability.md), so a deletion cannot commit
// unrecorded. The username is copied into the details first: after the DELETE
// there is nothing left to resolve the id against, and "user_deleted, target
// <uuid>" with no name is a trail that answers nothing.
func (s *Store) DeleteUser(ctx context.Context, actor, id uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var username string
		err := tx.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, id).Scan(&username)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: delete user: %w", err)
		}

		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("store: delete user: %w", err)
		}
		// Re-checked rather than assumed from the SELECT above. Between the
		// two statements another transaction may have committed the same
		// delete, and writing the audit row anyway would record a deletion
		// this request did not perform — while also reporting success for it.
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return writeAdminAudit(ctx, tx, actor, ActionUserDeleted, id.String(), AuditDetails{
			Username: username,
		})
	})
}

// ChangePassword replaces a user's password hash and revokes their sessions,
// in one transaction.
//
// keepSessionID names the one session that survives — the browser or device
// that made the request, for the self-service change of
// docs/specs/14-account-self-service.md. An empty string keeps none, which is
// the admin reset: the resetter cannot know which of the target's sessions are
// legitimate, so none of them are.
//
// **The revocation shares the transaction with the hash update by design.**
// Someone changes their password precisely when its secrecy is in doubt, so
// the old sessions are the thing being locked out; doing the delete as a
// best-effort follow-up would mean a failure between the two leaves the
// password changed and every previously-stolen session still working — the
// lock changed and the windows left open, with nothing in the response to say
// so.
//
// A user id that matches nothing is ErrNotFound, and the transaction rolls
// back, so no sessions are deleted for an id that named no account.
// It is the *self-service* change of spec 14 and is deliberately unaudited:
// admin_audit_log is the operator surface, and a person changing their own
// password is not an admin action (docs/specs/18-operations-and-observability.md).
// An admin resetting somebody else's goes through AdminResetPassword below.
func (s *Store) ChangePassword(ctx context.Context, userID uuid.UUID, passwordHash, keepSessionID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return changePassword(ctx, tx, userID, passwordHash, keepSessionID)
	})
}

// AdminResetPassword is the admin reset of docs/specs/14-account-self-service.md,
// audited. The hash update, the revocation of every one of the target's
// sessions, and the admin_audit_log row are one transaction.
//
// **The details record only who was reset**, never the new password or its
// hash — AuditDetails has no field either could occupy
// (internal/store/audit.go), so this is a property of the type rather than of
// this call site remembering.
func (s *Store) AdminResetPassword(ctx context.Context, actor, userID uuid.UUID, passwordHash string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// "" keeps no session: the resetter cannot know which of the target's
		// sessions are legitimate, so none of them are.
		if err := changePassword(ctx, tx, userID, passwordHash, ""); err != nil {
			return err
		}
		var username string
		if err := tx.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, userID).Scan(&username); err != nil {
			return fmt.Errorf("store: read reset target: %w", err)
		}
		return writeAdminAudit(ctx, tx, actor, ActionUserPasswordReset, userID.String(), AuditDetails{
			Username: username,
		})
	})
}

// changePassword is the statement pair both variants run.
func changePassword(ctx context.Context, tx pgx.Tx, userID uuid.UUID, passwordHash, keepSessionID string) error {
	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $1 WHERE id = $2`, passwordHash, userID)
	if err != nil {
		return fmt.Errorf("store: set password hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	// $2 = '' is never a real session id (they are 43-character base64url
	// tokens), so the empty string cleanly means "keep nothing" without a
	// second statement or a nullable parameter.
	if _, err := tx.Exec(ctx, `
		DELETE FROM sessions
		 WHERE user_id = $1 AND ($2 = '' OR id <> $2)`, userID, keepSessionID); err != nil {
		return fmt.Errorf("store: revoke sessions: %w", err)
	}
	return nil
}

// SetDisplayName renames a user, returning the updated row.
//
// Only the display name. Usernames are immutable — they are the login
// identifier, and renaming one buys a migration headache for no user-facing
// benefit (docs/specs/14-account-self-service.md).
func (s *Store) SetDisplayName(ctx context.Context, userID uuid.UUID, displayName string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		UPDATE users SET display_name = $1
		 WHERE id = $2
		RETURNING id, username, password_hash, display_name, is_admin, created_at`,
		displayName, userID))
}

// ListUsers returns every account, for the admin view.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, username, password_hash, display_name, is_admin, created_at
		  FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return out, nil
}

// CountUsers reports how many accounts exist, which is what the first-boot
// bootstrap tests before creating anything.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation
}

func scanUser(row rowScanner) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.IsAdmin, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		// Wrapped, so a caller can still reach a *pgconn.PgError underneath —
		// CreateUser uses that to turn a unique violation into ErrDuplicate.
		return nil, fmt.Errorf("store: scan user: %w", err)
	}
	return &u, nil
}
