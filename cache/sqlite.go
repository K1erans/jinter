package cache

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the driver
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	staleAfter    = 30 * 24 * time.Hour
	cleanupEvery  = 24 * time.Hour
	touchAfter    = 24 * time.Hour // last_used is refreshed at most this often
	lookupBatch   = 500
	busyTimeoutMS = 10000
)

// now is the clock; tests replace it.
var now = time.Now

const schema = `
CREATE TABLE IF NOT EXISTS answers (
	key       BLOB PRIMARY KEY,
	noul      REAL NOT NULL,
	model     TEXT NOT NULL,
	last_used INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS chunks (
	key       BLOB PRIMARY KEY,
	layout    BLOB NOT NULL,
	last_used INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS meta (
	name  TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);`

// SQLite is the on-disk Cache. WAL mode, a busy timeout and INSERT OR
// IGNORE let several processes share one database.
type SQLite struct {
	db    *sql.DB
	model string
}

// Dir is the folder holding cache databases: $JREP_CACHE_DIR, or jrep
// inside the user cache folder (~/Library/Caches/jrep on macOS). It is
// jrep's folder on purpose: the keys match, so an answer either one paid
// for serves both.
func Dir() (string, error) {
	if d := os.Getenv("JREP_CACHE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "jrep"), nil
}

// Path is the database file for the repo at root.
func Path(dir, root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".db")
}

// Clear deletes the database at path and its WAL files.
func Clear(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Open opens or creates the database at path. model is the version being
// searched with; once a day, rows from other versions and rows unused for
// 30 days are deleted.
func Open(path, model string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate",
		path, busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection per process: writers queue here instead of contending
	// for SQLite's lock.
	db.SetMaxOpenConns(1)
	c := &SQLite{db: db, model: model}
	// The first connection to a new file switches it to WAL, which needs an
	// exclusive lock and fails at once, without the busy timeout, while
	// another process holds the file. A failed connection isn't kept, so
	// trying again opens a fresh one.
	err = c.createSchema()
	for deadline := time.Now().Add(busyTimeoutMS * time.Millisecond); isBusy(err) && time.Now().Before(deadline); {
		time.Sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
		err = c.createSchema()
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening cache %s: %w", path, err)
	}
	if err := c.cleanup(); err != nil {
		db.Close()
		return nil, fmt.Errorf("cleaning cache %s: %w", path, err)
	}
	return c, nil
}

func (c *SQLite) Close() error { return c.db.Close() }

func isBusy(err error) bool {
	var se interface{ Code() int }
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// createSchema runs in a transaction, which takes the write lock up front
// (_txlock=immediate) and so waits out the busy timeout. On its own, each
// CREATE reads the schema and then upgrades to a write lock, and SQLite
// fails such an upgrade at once when another process is doing the same.
func (c *SQLite) createSchema() error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *SQLite) cleanup() error {
	t := now().Unix()
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last int64
	err = tx.QueryRow(`SELECT value FROM meta WHERE name = 'last_cleanup'`).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && t-last < int64(cleanupEvery/time.Second) {
		return nil
	}
	stale := t - int64(staleAfter/time.Second)
	if _, err := tx.Exec(`DELETE FROM answers WHERE last_used < ? OR model != ?`, stale, c.model); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chunks WHERE last_used < ?`, stale); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (name, value) VALUES ('last_cleanup', ?)`, t); err != nil {
		return err
	}
	return tx.Commit()
}

// Get returns the cached probability for every key it holds, and marks
// those rows as used.
func (c *SQLite) Get(keys []Key) (map[Key]float64, error) {
	return lookup[float64](c, "answers", "noul", keys)
}

// GetChunks returns the stored chunk layout for every key it holds, and
// marks those rows as used.
func (c *SQLite) GetChunks(keys []Key) (map[Key][]byte, error) {
	return lookup[[]byte](c, "chunks", "layout", keys)
}

// lookup reads column for keys from table in batches, refreshing last_used
// on rows that haven't been touched for a day. Keys are looked up in order,
// so each batch reads one narrow run of the index instead of pages from
// all over it.
func lookup[V any](c *SQLite, table, column string, keys []Key) (map[Key]V, error) {
	out := make(map[Key]V, len(keys))
	t := now().Unix()
	keys = slices.Clone(keys)
	slices.SortFunc(keys, func(a, b Key) int { return bytes.Compare(a[:], b[:]) })
	for len(keys) > 0 {
		batch := keys[:min(lookupBatch, len(keys))]
		keys = keys[len(batch):]
		args := make([]any, len(batch))
		for i, k := range batch {
			args[i] = k[:]
		}
		in := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		rows, err := c.db.Query(`SELECT key, `+column+`, last_used FROM `+table+` WHERE key IN (`+in+`)`, args...)
		if err != nil {
			return nil, err
		}
		var stale []any
		for rows.Next() {
			var raw []byte
			var v V
			var lastUsed int64
			if err := rows.Scan(&raw, &v, &lastUsed); err != nil {
				rows.Close()
				return nil, err
			}
			var k Key
			copy(k[:], raw)
			out[k] = v
			if t-lastUsed > int64(touchAfter/time.Second) {
				stale = append(stale, raw)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(stale) > 0 {
			in := strings.TrimSuffix(strings.Repeat("?,", len(stale)), ",")
			if _, err := c.db.Exec(`UPDATE `+table+` SET last_used = ? WHERE key IN (`+in+`)`, append([]any{t}, stale...)...); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Put saves one answer. An existing answer for the key is kept: the key
// already pins the exact model, question and code.
func (c *SQLite) Put(key Key, noul float64) error {
	_, err := c.db.Exec(`INSERT OR IGNORE INTO answers (key, noul, model, last_used) VALUES (?, ?, ?, ?)`,
		key[:], noul, c.model, now().Unix())
	return err
}

// PutChunks saves chunk layouts in one transaction. As with answers, an
// existing layout for a key is kept.
func (c *SQLite) PutChunks(layouts map[Key][]byte) error {
	if len(layouts) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO chunks (key, layout, last_used) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	t := now().Unix()
	for k, layout := range layouts {
		if _, err := stmt.Exec(k[:], layout, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}
