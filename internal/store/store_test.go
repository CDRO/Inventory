package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// TestSettingErrorClassification pins the two error shapes Setting must
// swallow into "no override".
//
// The undefined-table case is the load-bearing one: until the schema migration
// in spec 02 lands there is no settings table at all, so a fresh deployment
// hits SQLSTATE 42P01 on its first /healthz. Treating that as a hard failure
// makes the vision status permanently model_unavailable on a perfectly healthy
// install, and the suite would stay green because nothing else exercises it.
func TestSettingErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		err            error
		wantNoRows     bool
		wantUndefTable bool
	}{
		{
			name:       "no rows",
			err:        pgx.ErrNoRows,
			wantNoRows: true,
		},
		{
			name:       "no rows, wrapped",
			err:        fmt.Errorf("query settings: %w", pgx.ErrNoRows),
			wantNoRows: true,
		},
		{
			name:           "settings table does not exist yet",
			err:            &pgconn.PgError{Code: undefinedTable, Message: `relation "settings" does not exist`},
			wantUndefTable: true,
		},
		{
			name:           "undefined table, wrapped",
			err:            fmt.Errorf("query settings: %w", &pgconn.PgError{Code: undefinedTable}),
			wantUndefTable: true,
		},
		{
			name: "a different postgres error is a real failure",
			err:  &pgconn.PgError{Code: "42501", Message: "permission denied for table settings"},
		},
		{
			name: "a connection failure is a real failure",
			err:  errors.New("dial tcp: connection refused"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantNoRows, isNoRows(tc.err))
			assert.Equal(t, tc.wantUndefTable, isUndefinedTable(tc.err))
		})
	}
}

// TestUndefinedTableSQLState guards the constant itself. A typo here compiles,
// passes every other test, and turns a fresh deployment's fallback into a hard
// error.
func TestUndefinedTableSQLState(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "42P01", undefinedTable, "42P01 is PostgreSQL's undefined_table SQLSTATE")
}

// TestCloseIsSafeOnZeroValue documents that shutdown paths may call Close on a
// store that never opened, which happens when startup fails early.
func TestCloseIsSafeOnZeroValue(t *testing.T) {
	t.Parallel()

	var s *Store
	assert.NotPanics(t, func() { s.Close() })
	assert.NotPanics(t, func() { (&Store{}).Close() })
}
