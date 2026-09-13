package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/store"
)

func jobStatus(t *testing.T, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var status string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&status))
	return status
}

func TestJobLifecyclePendingToDone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)
	userID := newUser(t, ctx)

	job, err := s.CreateJob(ctx, storageID, store.JobShelfIngestion, &userID)
	require.NoError(t, err)
	assert.Equal(t, store.JobPending, job.Status)
	assert.Equal(t, "pending", jobStatus(t, ctx, job.ID))

	require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{"items":[1,2]}`)))

	got, err := s.Job(ctx, storageID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.JobDone, got.Status)
	assert.JSONEq(t, `{"items":[1,2]}`, string(got.Payload))
	assert.Nil(t, got.Error)
}

// TestOnlyAPendingJobCanFinish — a job discarded mid-flight, or already failed
// by the restart sweep, must not be resurrected by a vision call that returns
// late.
func TestOnlyAPendingJobCanFinish(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	job, err := s.CreateJob(ctx, storageID, store.JobProductPhoto, nil)
	require.NoError(t, err)
	require.NoError(t, s.FailJob(ctx, job.ID, "nope"))

	assert.ErrorIs(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{}`)), store.ErrNotFound)
	assert.Equal(t, "failed", jobStatus(t, ctx, job.ID), "a late result must not overwrite the failure")

	discarded, err := s.CreateJob(ctx, storageID, store.JobProductPhoto, nil)
	require.NoError(t, err)
	require.NoError(t, s.DeleteJob(ctx, storageID, discarded.ID))
	assert.ErrorIs(t, s.CompleteJob(ctx, discarded.ID, json.RawMessage(`{}`)), store.ErrNotFound)
}

// TestFailInterruptedJobsLeavesFinishedOnesAlone — the restart sweep fails what
// was pending and touches nothing else.
func TestFailInterruptedJobsLeavesFinishedOnesAlone(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	pending, err := s.CreateJob(ctx, storageID, store.JobShelfIngestion, nil)
	require.NoError(t, err)
	done, err := s.CreateJob(ctx, storageID, store.JobShelfIngestion, nil)
	require.NoError(t, err)
	require.NoError(t, s.CompleteJob(ctx, done.ID, json.RawMessage(`{"ok":true}`)))

	n, err := s.FailInterruptedJobs(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	got, err := s.Job(ctx, storageID, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.JobFailed, got.Status)
	require.NotNil(t, got.Error)
	assert.Equal(t, store.InterruptedJobError, *got.Error)

	assert.Equal(t, "done", jobStatus(t, ctx, done.ID), "a finished proposal survives a restart")
}

func TestJobsAreStorageScoped(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	mine := newStorage(t, ctx)
	theirs := newStorage(t, ctx)

	job, err := s.CreateJob(ctx, theirs, store.JobShelfIngestion, nil)
	require.NoError(t, err)

	_, err = s.Job(ctx, mine, job.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	assert.ErrorIs(t, s.DeleteJob(ctx, mine, job.ID), store.ErrNotFound)
	assert.Equal(t, "pending", jobStatus(t, ctx, job.ID), "a refused delete must not delete")

	listed, err := s.ListJobs(ctx, mine, []store.JobStatus{store.JobPending}, nil, 50)
	require.NoError(t, err)
	assert.Empty(t, listed)

	_, err = s.CreateJob(ctx, uuid.New(), store.JobShelfIngestion, nil)
	assert.ErrorIs(t, err, store.ErrNotFound, "a job for a storage that does not exist")
}

// TestListJobsPagesNewestFirstWithoutDrift — a job created between two page
// requests must not shift the second page, which is the offset failure mode.
func TestListJobsPagesNewestFirstWithoutDrift(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	var ids []uuid.UUID
	for range 5 {
		job, err := s.CreateJob(ctx, storageID, store.JobShelfIngestion, nil)
		require.NoError(t, err)
		ids = append(ids, job.ID)
	}
	consumed, err := s.CreateJob(ctx, storageID, store.JobShelfIngestion, nil)
	require.NoError(t, err)
	_, err = execTest(ctx, `UPDATE jobs SET status = 'consumed' WHERE id = $1`, consumed.ID)
	require.NoError(t, err)

	inbox := []store.JobStatus{store.JobPending, store.JobDone, store.JobFailed}

	first, err := s.ListJobs(ctx, storageID, inbox, nil, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, ids[4], first[0].ID, "newest first")
	assert.Equal(t, ids[3], first[1].ID)

	// Arrives between page requests.
	_, err = s.CreateJob(ctx, storageID, store.JobShelfIngestion, nil)
	require.NoError(t, err)

	second, err := s.ListJobs(ctx, storageID, inbox, &first[1].ID, 2)
	require.NoError(t, err)
	require.Len(t, second, 2)
	assert.Equal(t, ids[2], second[0].ID, "the page boundary does not move")
	assert.Equal(t, ids[1], second[1].ID)

	last, err := s.ListJobs(ctx, storageID, inbox, &second[1].ID, 2)
	require.NoError(t, err)
	require.Len(t, last, 1)
	assert.Equal(t, ids[0], last[0].ID)

	for _, page := range [][]store.Job{first, second, last} {
		for _, j := range page {
			assert.NotEqual(t, consumed.ID, j.ID, "a status filter excludes consumed jobs")
		}
	}
}

// TestConsumeJobAppliesAProposalOnce — the second confirm of the same job is a
// conflict, and a job that is not done cannot be consumed at all.
func TestConsumeJobAppliesAProposalOnce(t *testing.T) {
	s := requireDB(t)
	ctx := context.Background()
	storageID := newStorage(t, ctx)

	job, err := s.CreateJob(ctx, storageID, store.JobShelfIngestion, nil)
	require.NoError(t, err)

	consume := func(storage uuid.UUID) error {
		tx, err := testPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.ConsumeJob(ctx, tx, storage, job.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	assert.ErrorIs(t, consume(storageID), store.ErrConflict, "a pending job has nothing to apply")

	require.NoError(t, s.CompleteJob(ctx, job.ID, json.RawMessage(`{}`)))
	assert.ErrorIs(t, consume(newStorage(t, ctx)), store.ErrNotFound, "another storage's job")

	require.NoError(t, consume(storageID))
	assert.Equal(t, "consumed", jobStatus(t, ctx, job.ID))

	assert.ErrorIs(t, consume(storageID), store.ErrConflict, "a proposal cannot be applied twice")
}
