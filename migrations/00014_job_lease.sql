-- +goose Up

-- Which process owns a pending job (issue #121).
--
-- Recovery used to fail *every* pending row at start-up, on the assumption that
-- a pending row can only be one whose goroutine died with the previous process
-- (docs/specs/04-backend-api-conventions.md). That assumption holds for
-- stop-then-start and breaks for the rolling update on the NAS
-- (deploy/synology/update), which runs a second instance next to the first on
-- purpose: the new instance's start-up failed jobs the old one was still
-- working on, and the old one then finished the work, so the row and the
-- outcome disagreed.
--
-- With these two columns a pending row says who is working it and until when.
-- The owning process renews the claim while it lives (internal/jobs), so a
-- claim that has stopped being renewed is the only evidence recovery needs
-- that its owner is gone. A row with no claim at all — NULL — is by definition
-- one no goroutine is working, which is exactly the state that has to be
-- failed.
--
-- lease_owner is the owning *process*, not a user and not a container: it is a
-- fresh UUID per start-up, because a container that is restarted keeps its
-- hostname and its pid 1, and an identity a restarted process could mint again
-- would make it skip its own orphans forever. It is deliberately not a foreign
-- key to anything; there is no table of processes and nothing may cascade from
-- one.
ALTER TABLE jobs
    ADD COLUMN lease_owner      UUID,
    ADD COLUMN lease_expires_at TIMESTAMPTZ;

-- Both statements that read these columns — the recovery sweep and the owning
-- process's renewal — select pending rows and nothing else, while the table's
-- bulk is consumed and done rows that are never pending again. A partial index
-- keeps a sweep that runs every few seconds off the whole table.
CREATE INDEX idx_jobs_pending_leases ON jobs(lease_expires_at)
    WHERE status = 'pending';

-- Rows that predate the column get one short grace claim, with no owner to
-- renew it.
--
-- Without this, the update that applies this migration would reproduce the bug
-- one last time: the old instance's binary does not write a lease, so its
-- pending rows would arrive at the new instance's recovery with a NULL claim
-- and be failed — the exact race this migration exists to close. The grace
-- claim has to outlast only the start-up of the release that applies it, after
-- which the sweep fails whatever is still pending, so it is deliberately as
-- short as the lease TTL in internal/jobs (one minute). It errs towards
-- failing an orphan a minute late rather than failing a live job at once,
-- which is the trade the whole feature makes.
UPDATE jobs SET lease_expires_at = now() + interval '1 minute'
 WHERE status = 'pending';

-- +goose Down

DROP INDEX IF EXISTS idx_jobs_pending_leases;

ALTER TABLE jobs
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_owner;
