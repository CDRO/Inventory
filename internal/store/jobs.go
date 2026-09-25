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

// JobLease is a process's claim on a pending job: who is working it, and how
// long the claim stands without being renewed (issue #121).
//
// The zero value is "unowned", and that is a meaningful state rather than a
// mistake: a pending job nobody claims is by definition one no goroutine is
// working, which is exactly what FailOrphanedJobs exists to fail. Only
// internal/jobs mints these, because only a process that will renew a claim may
// make one.
type JobLease struct {
	// Owner identifies the process, not the person and not the container. It
	// is minted fresh at every start-up: an identity a restarted process could
	// mint again would make that process skip its own orphaned rows.
	Owner uuid.UUID
	// TTL is how long the claim outlives its last renewal. Ignored when Owner
	// is the zero UUID.
	TTL time.Duration
}

// claim returns the two query arguments a lease writes: the owner, and the TTL
// in seconds for the database to add to its own clock.
//
// Both are nil for an unowned lease, and the statements below add the seconds
// with make_interval, where a NULL argument yields a NULL interval and so a NULL
// expiry. An insert carrying no lease is therefore indistinguishable from a row
// that predates these columns — both are orphans, which is correct.
//
// The expiry is computed from the database's clock, never this process's: it is
// only ever compared in SQL, and two app instances must not have to agree on the
// time for one to see that the other's claim is still current.
func (l JobLease) claim() (owner *uuid.UUID, seconds *float64) {
	if l.Owner == uuid.Nil {
		return nil, nil
	}
	secs := l.TTL.Seconds()
	return &l.Owner, &secs
}

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
	// ImageFilename is the photo behind a photo job, under the ingest upload
	// area. Nil for a job with no image, or once retention removed it.
	ImageFilename *string
	// LocationHintID is the shelf the photo was taken for, if the user was
	// scoped into one.
	LocationHintID *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewJob is the input to CreateJob.
type NewJob struct {
	StorageID      uuid.UUID
	Kind           JobKind
	CreatedBy      *uuid.UUID
	ImageFilename  *string
	LocationHintID *uuid.UUID
	// Lease claims the job for the process that is about to work it, in the
	// same statement that inserts it — there is no window in which the row is
	// pending and unclaimed. internal/jobs fills this in; a caller that leaves
	// it zero gets an unowned job, which the next recovery sweep fails.
	Lease JobLease
}

const jobColumns = `id, storage_id, kind, status, payload, error, created_by, image_filename, location_hint_id, created_at, updated_at`

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.StorageID, &j.Kind, &j.Status, &j.Payload, &j.Error, &j.CreatedBy,
		&j.ImageFilename, &j.LocationHintID, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan job: %w", err)
	}
	return &j, nil
}

// CreateJob inserts a pending job.
//
// A location hint is validated against the job's storage: a shelf in another
// storage is ErrNotFound, exactly like one that does not exist
// (docs/specs/06-vision-shelf-ingestion.md). So is a storage that does not
// exist.
func (s *Store) CreateJob(ctx context.Context, in NewJob) (*Job, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}

	var out *Job
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if in.LocationHintID != nil {
			if err := requireSameStorage(ctx, tx, treeLocations, in.StorageID, *in.LocationHintID); err != nil {
				return err
			}
		}
		owner, secs := in.Lease.claim()
		job, err := scanJob(tx.QueryRow(ctx, `
			INSERT INTO jobs (id, storage_id, kind, status, created_by, image_filename, location_hint_id,
			                  lease_owner, lease_expires_at)
			VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, now() + make_interval(secs => $8))
			RETURNING `+jobColumns, id, in.StorageID, in.Kind, in.CreatedBy, in.ImageFilename, in.LocationHintID,
			owner, secs))
		if isForeignKeyViolation(err) {
			return ErrNotFound
		}
		out = job
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExpiredJobImages returns the image filenames of consumed jobs last touched
// before cutoff — the photos whose review is over and whose 30-day grace has
// passed (docs/specs/04-backend-api-conventions.md).
//
// Only consumed jobs. A done job waits for review indefinitely, and its photo
// has to be there when it does; a discarded job's photo is deleted with it.
func (s *Store) ExpiredJobImages(ctx context.Context, cutoff time.Time, limit int) (map[uuid.UUID]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, image_filename FROM jobs
		 WHERE status = 'consumed' AND image_filename IS NOT NULL AND updated_at < $1
		 ORDER BY updated_at
		 LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("store: expired job images: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: scan expired job image: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ClearJobImage records that a job's photo is gone from disk.
func (s *Store) ClearJobImage(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `UPDATE jobs SET image_filename = NULL WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: clear job image: %w", err)
	}
	return nil
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

// finishJob moves a pending job to its outcome and drops its lease with it: the
// claim only ever means "this process is working this pending row", so a job
// that is no longer pending must not still name an owner.
func (s *Store) finishJob(ctx context.Context, id uuid.UUID, status JobStatus, payload json.RawMessage, message *string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = $2, payload = $3, error = $4, updated_at = now(),
		                lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = $1 AND status = 'pending'`, id, status, payload, message)
	if err != nil {
		return fmt.Errorf("store: finish job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailOrphanedJobs marks failed every pending job whose owner is gone: one
// whose claim has lapsed, and one that was never claimed at all. Left alone
// either would poll as "pending" forever
// (docs/specs/04-backend-api-conventions.md).
//
// owner is the calling process's own lease owner, and its rows are the one
// exception: a claim of ours that has lapsed means our own renewal did not get
// through — a slow query, a brief database stall — not that the goroutine
// behind it is gone. Failing our own live work on that evidence would be the
// same mistake this function was written to stop, in one process instead of
// two. Another instance, which cannot tell a stalled renewal from a dead
// process, does fail those rows; that is what makes the work of an instance
// killed outright recoverable at all (issue #124, item 4).
//
// It replaces FailInterruptedJobs, which did this unconditionally and was
// wrong to: it ran at start-up on the assumption that nothing else could be
// working a pending row, which the rolling update on the NAS breaks on purpose
// by running a second instance next to the first (deploy/synology/update,
// issue #121).
//
// It runs both before the listener and on a timer while the process lives, so
// it must stay one indexed statement.
func (s *Store) FailOrphanedJobs(ctx context.Context, owner uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'failed', error = $1, updated_at = now(),
		                lease_owner = NULL, lease_expires_at = NULL
		 WHERE status = 'pending'
		   AND lease_owner IS DISTINCT FROM $2
		   AND (lease_expires_at IS NULL OR lease_expires_at < now())`, InterruptedJobError, owner)
	if err != nil {
		return 0, fmt.Errorf("store: fail orphaned jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RenewJobLeases pushes the expiry of every pending job lease.Owner holds out
// by lease.TTL, and reports how many it renewed.
//
// This is the heartbeat, and it covers queued jobs as much as running ones: a
// job waiting for a concurrency slot is legitimately pending for as long as the
// queue ahead of it takes, which no fixed age bound can predict, and it is the
// case a bound-based recovery would wrongly fail.
//
// updated_at is deliberately untouched. It is the job's own last change, which
// the retention sweep and every client reading a job order by; a claim renewed
// every few seconds is bookkeeping about the process, not a change to the job.
func (s *Store) RenewJobLeases(ctx context.Context, lease JobLease) (int64, error) {
	owner, secs := lease.claim()
	if owner == nil {
		return 0, fmt.Errorf("%w: renewing a lease needs an owner", ErrValidation)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET lease_expires_at = now() + make_interval(secs => $2)
		 WHERE status = 'pending' AND lease_owner = $1`, owner, secs)
	if err != nil {
		return 0, fmt.Errorf("store: renew job leases: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseJobLeases drops owner's claim on every pending job it still holds,
// leaving the rows pending and unowned, and reports how many it let go.
//
// A process calls this as it shuts down, for the rows whose outcome it could not
// record inside its grace period. It is what keeps stop-then-start behaving as
// it always has: an unowned pending row is an orphan, so the next start-up's
// recovery fails it at once rather than waiting out a claim nobody will renew.
//
// It is an optimisation, not the guarantee — a process killed outright never
// reaches it, and the timed sweep is what covers that.
func (s *Store) ReleaseJobLeases(ctx context.Context, owner uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET lease_owner = NULL, lease_expires_at = NULL
		 WHERE status = 'pending' AND lease_owner = $1`, owner)
	if err != nil {
		return 0, fmt.Errorf("store: release job leases: %w", err)
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
//
// It returns the job's image filename, if it had one, so the caller can delete
// the photo too: discarding a job "drops the proposal and its image"
// (docs/specs/06-vision-shelf-ingestion.md). The row goes first, so a failure
// to remove the file leaves an orphan on disk rather than a job pointing at a
// missing photo.
func (s *Store) DeleteJob(ctx context.Context, storageID, id uuid.UUID) (*string, error) {
	var image *string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM jobs WHERE id = $1 AND storage_id = $2 RETURNING image_filename`, id, storageID).Scan(&image)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: delete job: %w", err)
	}
	return image, nil
}

// DiscardedJob is one row DiscardJobs deleted, holding what the caller needs
// for file clean-up afterwards.
type DiscardedJob struct {
	ID            uuid.UUID
	ImageFilename *string
}

// DiscardJobs deletes every job of storageID whose status is pending, done or
// failed and whose created_at is at or before upTo, in one statement — "the
// whole inbox" (docs/specs/32-inbox-discard-all.md). Consumed jobs are never
// touched; the retention sweep owns them.
//
// upTo is a server timestamp the caller captured earlier, never the device
// clock: it is what closes the race where another member uploads a photo
// between the inbox being rendered and the bulk discard being confirmed. The
// returned rows' image filenames are what the caller needs to remove the
// photos; cutout clean-up goes by job id alone, so no more of the row is
// returned than that.
func (s *Store) DiscardJobs(ctx context.Context, storageID uuid.UUID, upTo time.Time) ([]DiscardedJob, error) {
	rows, err := s.pool.Query(ctx, `
		DELETE FROM jobs
		 WHERE storage_id = $1
		   AND status IN ('pending', 'done', 'failed')
		   AND created_at <= $2
		RETURNING id, image_filename`, storageID, upTo)
	if err != nil {
		return nil, fmt.Errorf("store: discard jobs: %w", err)
	}
	defer rows.Close()

	var out []DiscardedJob
	for rows.Next() {
		var d DiscardedJob
		if err := rows.Scan(&d.ID, &d.ImageFilename); err != nil {
			return nil, fmt.Errorf("store: scan discarded job: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: discard jobs: %w", err)
	}
	return out, nil
}

// RequeueJob moves a done or failed job back to pending, dropping its proposal
// and its error, so its photo can be analysed again — "Analyze again"
// (docs/specs/09-consumption-logging.md).
//
// Only a job that has finished and still has its photo moves. A pending job is
// already being analysed, and a consumed one has been applied: both are
// ErrConflict, as is a job with no photo to analyse. A job in another storage,
// or none at all, is ErrNotFound.
//
// The row is locked for the check, so a confirm racing a re-analysis ends one
// way or the other: ConsumeJob takes the same lock and finds the job pending,
// or this finds it consumed.
//
// lease claims the job for the process that is about to re-run its vision call,
// in the same statement and for the same reason as CreateJob: a requeued job is
// pending again, and a pending row with no owner is an orphan.
func (s *Store) RequeueJob(ctx context.Context, storageID, id uuid.UUID, lease JobLease) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var status JobStatus
		var image *string
		err := tx.QueryRow(ctx, `
			SELECT status, image_filename FROM jobs WHERE id = $1 AND storage_id = $2 FOR UPDATE`,
			id, storageID).Scan(&status, &image)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: lock job: %w", err)
		}
		if status != JobDone && status != JobFailed {
			return fmt.Errorf("%w: job is %s, not done or failed", ErrConflict, status)
		}
		if image == nil {
			return fmt.Errorf("%w: job has no photo", ErrConflict)
		}
		owner, secs := lease.claim()
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET status = 'pending', payload = NULL, error = NULL, updated_at = now(),
			                lease_owner = $2, lease_expires_at = now() + make_interval(secs => $3)
			 WHERE id = $1`, id, owner, secs); err != nil {
			return fmt.Errorf("store: requeue job: %w", err)
		}
		return nil
	})
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
