package jobs

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCommitOutcomeProtectsReferencedFiles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		uncertain bool
	}{
		{"success", nil, false},
		{"explicit rollback", fmt.Errorf("commit: %w", pgx.ErrTxCommitRollback), false},
		{"constraint rollback", &pgconn.PgError{Code: "23514"}, false},
		{"serialization rollback", &pgconn.PgError{Code: "40001"}, false},
		{"socket lost", io.EOF, true},
		{"cancelled commit", context.Canceled, true},
		{"connection error reply", &pgconn.PgError{Code: "08006"}, true},
		{"server outcome unknown", &pgconn.PgError{Code: "40003"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitOutcomeUncertain(tc.err); got != tc.uncertain {
				t.Fatalf("uncertain=%v want %v", got, tc.uncertain)
			}
		})
	}
}
