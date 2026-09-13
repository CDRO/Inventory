-- +goose Up

-- Photo ingestion (docs/specs/06-vision-shelf-ingestion.md) needs two things
-- on a job that the generic jobs table did not carry.

-- The uploaded photo behind the proposal. A generated filename under
-- /data/uploads/ingest (docs/specs/04-backend-api-conventions.md), never a path
-- and never anything the client named. It is kept for as long as the job is
-- unreviewed, so crops still render weeks later, and NULLed once the retention
-- sweep has removed the file.
ALTER TABLE jobs ADD COLUMN image_filename TEXT;

-- The shelf the user was scoped into when they took the photo, if any. It is
-- the default location for rows the model could not place. ON DELETE SET NULL:
-- deleting a shelf must not delete a proposal waiting for review, and the
-- reviewer picks a location at confirm time either way.
ALTER TABLE jobs ADD COLUMN location_hint_id UUID REFERENCES locations(id) ON DELETE SET NULL;

-- The retention sweep looks for consumed jobs whose image is still on disk.
CREATE INDEX idx_jobs_consumed_images ON jobs(updated_at)
    WHERE status = 'consumed' AND image_filename IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_jobs_consumed_images;
ALTER TABLE jobs DROP COLUMN IF EXISTS location_hint_id;
ALTER TABLE jobs DROP COLUMN IF EXISTS image_filename;
