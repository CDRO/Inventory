package uploads

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// CutoutsDir is where background-removed pictures wait while their job is
// under review (docs/specs/09-consumption-logging.md). Each is offered next to
// the original and becomes a product picture only if the reviewer keeps it, so
// none of them outlives the review: a job's cutouts are removed when it is
// confirmed, discarded, or analysed again.
const CutoutsDir = "/data/uploads/cutouts"

// JobDirs is an upload area with one directory per job, so every file made for
// a job can be removed with it in one call, without anything having to record
// their names.
//
// The directory is named by the job's id — a uuid.UUID, never a string from a
// request — and the files in it follow Dir's generated-name rule.
type JobDirs struct {
	root string
}

// NewJobDirs returns the area at root, creating it if needed. The per-job
// directories are created on first save.
func NewJobDirs(root string) (*JobDirs, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("uploads: create %s: %w", root, err)
	}
	return &JobDirs{root: root}, nil
}

func (d *JobDirs) dir(job uuid.UUID) *Dir {
	return &Dir{root: filepath.Join(d.root, job.String()), names: generatedName}
}

// Save writes a file for a job.
func (d *JobDirs) Save(job uuid.UUID, name string, data []byte) error {
	dir, err := NewDir(filepath.Join(d.root, job.String()))
	if err != nil {
		return err
	}
	return dir.Save(name, data)
}

// Read returns a job's file. A missing file, or a job with no files at all, is
// os.ErrNotExist.
func (d *JobDirs) Read(job uuid.UUID, name string) ([]byte, error) {
	return d.dir(job).Read(name)
}

// RemoveAll deletes every file of a job. A job with none is not an error.
func (d *JobDirs) RemoveAll(job uuid.UUID) error {
	if err := os.RemoveAll(filepath.Join(d.root, job.String())); err != nil {
		return fmt.Errorf("uploads: remove job files: %w", err)
	}
	return nil
}
