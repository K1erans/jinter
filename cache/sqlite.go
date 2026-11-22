package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
	put   *sql.Stmt // PutAnswers' INSERT, prepared once: it runs once per Jev call
}

// Dir is the folder holding cache databases: $JINTER_CACHE_DIR, or jinter
// inside the user cache folder (~/Library/Caches/jinter on macOS).
func Dir() (string, error) {
	if d := os.Getenv("JINTER_CACHE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "jinter"), nil
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
		uriPath.Replace(path), busyTimeoutMS)
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
	if c.put, err = db.Prepare(`INSERT OR IGNORE INTO answers (key, noul, model, last_used) VALUES (?, ?, ?, ?)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening cache %s: %w", path, err)
	}
	return c, nil
}

// uriPath escapes the characters SQLite's URI filenames give a meaning to,
// so a path holding them names the file it says: unescaped, "?" or "#"
// would cut the path short (and drop the pragmas after it) and "%" would
// be decoded.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

func (c *SQLite) Close() error {
	return errors.Join(c.put.Close(), c.db.Close())
}

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

// lookup reads column for keys from table, then refreshes last_used on
// the rows found that haven't been touched for a day. Keys are looked up
// in order, and once each, so each batch reads one narrow run of the index
// instead of pages from all over it.
//
// The reads share one read transaction, which takes no write lock, and
// the refresh is one write transaction afterwards: the cache has a single
// connection, so the two can't overlap.
func lookup[V any](c *SQLite, table, column string, keys []Key) (map[Key]V, error) {
	if len(keys) == 0 {
		return map[Key]V{}, nil
	}
	keys = slices.Clone(keys)
	slices.SortFunc(keys, compareKeys)
	keys = slices.Compact(keys)
	t := now().Unix()

	tx, err := c.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make(map[Key]V, len(keys))
	var stale []Key
	query := func(n int) string {
		return `SELECT key, ` + column + `, last_used < ? FROM ` + table + ` WHERE key IN (` + params(n) + `)`
	}
	err = inBatches(tx, keys, t-int64(touchAfter/time.Second), query, func(stmt *sql.Stmt, args []any) error {
		rows, err := stmt.Query(args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw sql.RawBytes // only valid until Next; copied into k
			var v V
			var old bool
			if err := rows.Scan(&raw, &v, &old); err != nil {
				return err
			}
			var k Key
			copy(k[:], raw)
			out[k] = v
			if old {
				stale = append(stale, k)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	tx.Rollback() // release the connection for the refresh
	if len(stale) > 0 {
		if err := c.touch(table, stale, t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// touch sets last_used to t on table's rows for keys, in one transaction.
func (c *SQLite) touch(table string, keys []Key, t int64) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := func(n int) string { return `UPDATE ` + table + ` SET last_used = ? WHERE key IN (` + params(n) + `)` }
	err = inBatches(tx, keys, t, query, func(stmt *sql.Stmt, args []any) error {
		_, err := stmt.Exec(args...)
		return err
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

// inBatches calls fn for keys lookupBatch at a time, with the statement
// query(n) for a batch of n keys and arguments first, then the batch's
// keys. Every batch but the last is full, so at most two statements are
// prepared; they close with tx.
func inBatches(tx *sql.Tx, keys []Key, first any, query func(n int) string, fn func(*sql.Stmt, []any) error) error {
	var stmt *sql.Stmt
	prepared := 0 // batch size stmt was prepared for
	args := make([]any, 0, 1+min(lookupBatch, len(keys)))
	for batch := range slices.Chunk(keys, lookupBatch) {
		if len(batch) != prepared {
			var err error
			if stmt, err = tx.Prepare(query(len(batch))); err != nil {
				return err
			}
			prepared = len(batch)
		}
		args = append(args[:0], first)
		for i := range batch {
			args = append(args, batch[i][:])
		}
		if err := fn(stmt, args); err != nil {
			return err
		}
	}
	return nil
}

func compareKeys(a, b Key) int { return bytes.Compare(a[:], b[:]) }

// params is n comma-separated placeholders.
func params(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// PutAnswers saves answers in one transaction, which a search's workers
// take turns at: one per Jev call rather than one per answer. An existing
// answer for a key is kept, and so is its last_used: the key already pins
// the exact model, question and code. A key repeated within answers is
// likewise written once.
//
// Each new key most likely lands on a page of its own, and every page a
// transaction changes is written out when it commits, so saving many
// answers at once costs less per answer only until the table is large.
func (c *SQLite) PutAnswers(answers []Answer) error {
	if len(answers) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The cache has one connection, on which c.put is already prepared,
	// so this reuses it rather than preparing it again.
	stmt := tx.Stmt(c.put)
	t := now().Unix()
	for _, a := range answers {
		if _, err := stmt.Exec(a.Key[:], a.Noul, c.model, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PutChunks saves chunk layouts in one transaction. As with answers, an
// existing layout for a key is kept. Layouts are inserted in key order,
// which fills the table's pages one after another instead of at random.
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
	keys := slices.SortedFunc(maps.Keys(layouts), compareKeys)
	for _, k := range keys {
		if _, err := stmt.Exec(k[:], layouts[k], t); err != nil {
			return err
		}
	}
	return tx.Commit()
}
