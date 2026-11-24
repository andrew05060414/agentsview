package db

import (
	"context"
	"fmt"
)

const AccountAliasKeyPrefix = "account_alias:"

func (db *DB) GetAccountAliases(context.Context) (map[string]string, error) {
	return nil, nil
}

// BackfillSessionAccount labels sessions after the browser has confirmed
// the same identity at both ends of a sync round.
func (db *DB) BackfillSessionAccount(ctx context.Context, account string, ids []string) error {
	if account == "" || len(ids) == 0 {
		return nil
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		in, args := InPlaceholders(ids[start:min(start+batchSize, len(ids))])
		args = append([]any{account}, args...)
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET account = ?,
				local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE account = '' AND deleted_at IS NULL AND id IN `+in, args...); err != nil {
			return fmt.Errorf("backfilling session account: %w", err)
		}
	}
	return tx.Commit()
}
