package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// JobKind is what a background job does. The values are the ones jobs.kind
// accepts.
type JobKind string

const (
	JobShelfIngestion    JobKind = "shelf_ingestion"
	JobProductPhoto      JobKind = "product_photo"
	JobShoppingListPhoto JobKind = "shopping_list_photo"
	JobConsumptionPhoto  JobKind = "consumption_photo"
)

// JobStatus is where a job is in its life
// (docs/specs/04-backend-api-conventions.md).
type JobStatus string

const (
	// JobPending is waiting for, or in the middle of, its vision call.
	JobPending JobStatus = "pending"
	// JobDone holds a proposal that nobody has confirmed or discarded yet. It
	// waits indefinitely: there is no review deadline.
	JobDone JobStatus = "done"
	// JobFailed ended with an error the user can read.
	JobFailed JobStatus = "failed"
	// JobConsumed has had its proposal applied, so it cannot be applied twice.
	JobConsumed JobStatus = "consumed"
)

// Valid reports whether s is a status jobs.status accepts.
func (s JobStatus) Valid() bool {
	switch s {
	case JobPending, JobDone, JobFailed, JobConsumed:
		return true
	}
	return false
}

// InterruptedJobError is the message a job gets when the server stopped before
// it finished. It says what the user can do about it, because they will read
// it in the review inbox with no other context.
const InterruptedJobError = "Processing was interrupted by a server restart. Upload the photo again."

// Job is one row of jobs.
type Job struct {
	ID        uuid.UUID
	StorageID uuid.UUID
	Kind      JobKind
	Status    JobStatus
	// Payload is the parsed proposal, present once the job is done.
	Payload   json.RawMessage
	Error     *string
	CreatedBy *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

const jobColumns = `id, storage_id, kind, status, payload, error, created_by, created_at, updated_at`

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.StorageID, &j.Kind, &j.Status, &j.Payload, &j.Error, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan job: %w", err)
	}
	return &j, nil
}

// CreateJob inserts a pending job.
func (s *Store) CreateJob(ctx context.Context, storageID uuid.UUID, kind JobKind, createdBy *uuid.UUID) (*Job, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	job, err := scanJob(s.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, storage_id, kind, status, created_by)
		VALUES ($1, $2, $3, 'pending', $4)
		RETURNING `+jobColumns, id, storageID, kind, createdBy))
	if isForeignKeyViolation(err) {
		return nil, ErrNotFound
	}
	return job, err
}

// CompleteJob records a proposal and moves a pending job to done.
//
// Only a pending job moves. A job discarded while its vision call was still
// running is gone, and one already failed by the restart sweep stays failed:
// both are ErrNotFound here, and the runner treats that as "nobody is waiting
// for this result any more" rather than as an error.
func (s *Store) CompleteJob(ctx context.Context, id uuid.UUID, payload json.RawMessage) error {
	return s.finishJob(ctx, id, JobDone, payload, nil)
}

// FailJob moves a pending job to failed with a message the user will read.
func (s *Store) FailJob(ctx context.Context, id uuid.UUID, message string) error {
	return s.finishJob(ctx, id, JobFailed, nil, &message)
}

func (s *Store) finishJob(ctx context.Context, id uuid.UUID, status JobStatus, payload json.RawMessage, message *string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = $2, payload = $3, error = $4, updated_at = now()
		 WHERE id = $1 AND status = 'pending'`, id, status, payload, message)
	if err != nil {
		return fmt.Errorf("store: finish job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailInterruptedJobs marks every pending job failed, for startup.
//
// It runs before the server accepts requests, when no goroutine can be working
// on anything yet — so every pending row is by definition one whose goroutine
// died with the previous process. Left alone it would poll as "pending"
// forever (docs/specs/04-backend-api-conventions.md).
func (s *Store) FailInterruptedJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'failed', error = $1, updated_at = now()
		 WHERE status = 'pending'`, InterruptedJobError)
	if err != nil {
		return 0, fmt.Errorf("store: fail interrupted jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Job returns one job, scoped to a storage. A job in another storage is
// ErrNotFound, exactly like one that does not exist.
func (s *Store) Job(ctx context.Context, storageID, id uuid.UUID) (*Job, error) {
	return scanJob(s.pool.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1 AND storage_id = $2`, id, storageID))
}

// ListJobs returns a storage's jobs in the given statuses, newest first, one
// page at a time.
//
// The cursor is the id of the last job on the previous page. Job ids are
// UUIDv7, so id order is creation order and the page boundary does not move
// when new jobs arrive — which an offset would
// (docs/specs/04-backend-api-conventions.md).
func (s *Store) ListJobs(ctx context.Context, storageID uuid.UUID, statuses []JobStatus, after *uuid.UUID, limit int) ([]Job, error) {
	if len(statuses) == 0 {
		return nil, fmt.Errorf("%w: at least one job status is required", ErrValidation)
	}
	names := make([]string, len(statuses))
	for i, st := range statuses {
		names[i] = string(st)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+jobColumns+` FROM jobs
		 WHERE storage_id = $1
		   AND status = ANY($2)
		   AND ($3::uuid IS NULL OR id < $3)
		 ORDER BY id DESC
		 LIMIT $4`, storageID, names, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	return out, nil
}

// DeleteJob discards a job, scoped to a storage.
func (s *Store) DeleteJob(ctx context.Context, storageID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1 AND storage_id = $2`, id, storageID)
	if err != nil {
		return fmt.Errorf("store: delete job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ConsumeJob marks a done job consumed, so its proposal cannot be applied twice.
//
// It is the confirm endpoint's guard (docs/specs/06-vision-shelf-ingestion.md):
// a second confirm of the same job — a double click, a retry, two members
// reviewing at once — finds the job no longer done and gets ErrConflict. A
// job in another storage, or none at all, is ErrNotFound.
//
// The confirm endpoints of spec 06 must call this inside the same transaction
// as the writes the proposal causes, or a crash between the two could apply a
// proposal without consuming it. They take a pgx.Tx for that reason.
func ConsumeJob(ctx context.Context, tx pgx.Tx, storageID, id uuid.UUID) error {
	var status JobStatus
	err := tx.QueryRow(ctx, `
		SELECT status FROM jobs WHERE id = $1 AND storage_id = $2 FOR UPDATE`, id, storageID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock job: %w", err)
	}
	if status != JobDone {
		return fmt.Errorf("%w: job is %s, not done", ErrConflict, status)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status = 'consumed', updated_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: consume job: %w", err)
	}
	return nil
}
