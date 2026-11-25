package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestRawDeviceAuthUniqueViolationRejectsTypedNil(t *testing.T) {
	t.Parallel()

	var pgErr *pgconn.PgError
	var err error = pgErr
	assert.NotPanics(t, func() {
		assert.False(t, rawDeviceAuthUniqueViolation(err))
	})
}

// TestRawDeviceAuthUniqueViolation pins the only code that turns a Postgres
// unique-constraint failure into rawsync.ErrConflict: code 23505, reachable
// through wrapping. Anything else must stay an ordinary database error so
// callers do not take the conflict branch by mistake.
func TestRawDeviceAuthUniqueViolation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unique violation", &pgconn.PgError{Code: "23505"}, true},
		{
			"wrapped unique violation",
			fmt.Errorf("insert raw_devices: %w", &pgconn.PgError{Code: "23505"}),
			true,
		},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, false},
		{"pg error without code", &pgconn.PgError{}, false},
		{"plain error", errors.New("connection reset"), false},
		{"nil error", nil, false},
		{
			"wrapped typed nil",
			fmt.Errorf("insert: %w", (*pgconn.PgError)(nil)),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, rawDeviceAuthUniqueViolation(tc.err))
		})
	}
}
