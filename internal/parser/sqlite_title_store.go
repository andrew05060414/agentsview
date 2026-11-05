package parser

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/stringutil"
)

// sqliteTitleSnapshot is one cached read of a native title database.
type sqliteTitleSnapshot struct {
	state  SQLiteContainerState
	titles map[string]string
	err    error
	readAt time.Time
}

var sqliteTitleNow = time.Now

// One cache serves every native title database. Two copies of the expiry and
// container-state rules would drift apart.
var sqliteTitleCache = struct {
	sync.Mutex
	stores map[string]sqliteTitleSnapshot
}{stores: make(map[string]sqliteTitleSnapshot)}

// InvalidateSQLiteTitles expires the cached read, retaining its container state.
func InvalidateSQLiteTitles(path string) {
	sqliteTitleCache.Lock()
	defer sqliteTitleCache.Unlock()
	if prior, cached := sqliteTitleCache.stores[path]; cached {
		prior.readAt = time.Time{}
		sqliteTitleCache.stores[path] = prior
	}
}

// sqliteStoreTitles reads a native title database, reusing the last result
// while the container state matches. Matching markers can hide writes, so
// cached titles expire.
func sqliteStoreTitles(
	ctx context.Context, path, table, idColumn, query string,
) (map[string]string, SQLiteContainerState, error) {
	state, ok := StatSQLiteContainerState(path)
	sqliteTitleCache.Lock()
	defer sqliteTitleCache.Unlock()
	now := sqliteTitleNow()
	prior, cached := sqliteTitleCache.stores[path]
	if ok && cached &&
		prior.state == state &&
		now.Sub(prior.readAt) < SQLiteContainerDigestVerifyInterval {
		return prior.titles, prior.state, prior.err
	}
	titles, err := readSQLiteTitles(ctx, path, table, idColumn, query, !cached || prior.state != state)
	if ctx.Err() != nil {
		return nil, state, ctx.Err()
	}
	if ok {
		// Remote imports extract stores into fresh temp roots; bound what stays cached.
		if _, cached := sqliteTitleCache.stores[path]; !cached && len(sqliteTitleCache.stores) >= 16 {
			clear(sqliteTitleCache.stores)
		}
		sqliteTitleCache.stores[path] = sqliteTitleSnapshot{
			state: state, titles: titles, err: err, readAt: now,
		}
	} else {
		delete(sqliteTitleCache.stores, path)
	}
	return titles, state, err
}

func readSQLiteTitles(
	ctx context.Context, path, table, idColumn, query string,
	stateChanged bool,
) (map[string]string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	observeSharedContainerScan(ctx)
	db, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var exists, hasID, hasTitle bool
	if err := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?),
		EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?),
		EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = 'title')`, table, table, idColumn, table).Scan(&exists, &hasID, &hasTitle); err != nil {
		return nil, err
	}
	if missing := missingTitleSchema(table, idColumn, exists, hasID, hasTitle); missing != "" {
		if stateChanged {
			log.Printf("Warning: title database %s is missing %s", path, missing)
		}
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	titles := make(map[string]string)
	for rows.Next() {
		var id string
		var title sql.NullString
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		if !title.Valid {
			continue
		}
		titles[id] = strings.TrimSpace(stringutil.SanitizeUTF8(title.String))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return titles, nil
}

// missingTitleSchema names the first absent part of a title schema, or "".
func missingTitleSchema(table, idColumn string, exists, hasID, hasTitle bool) string {
	switch {
	case !exists:
		return "table " + table
	case !hasID:
		return "column " + idColumn
	case !hasTitle:
		return "column title"
	}
	return ""
}
