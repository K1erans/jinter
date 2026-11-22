package cache

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/libc/sys/types"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	staleAfter    = 30 * 24 * time.Hour
	cleanupEvery  = 24 * time.Hour
	touchAfter    = 24 * time.Hour // last_used is refreshed at most this often
	lookupBatch   = 1000
	busyTimeoutMS = 10000
)

// Batch sizes used for multi-row INSERT statements, largest first. Using a
// small fixed ladder means at most 5 statements are ever prepared per table
// while cutting per-row step/reset overhead by ~100-500x.
var (
	answerInsertSizes = [...]int{250, 64, 16, 4, 1}
	chunkInsertSizes  = [...]int{500, 128, 32, 8, 1}
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

// sqliteError wraps a SQLite result code so isBusy can inspect it.
type sqliteError struct {
	code int32
	msg  string
}

func (e *sqliteError) Error() string { return e.msg }
func (e *sqliteError) Code() int     { return int(e.code) }

// putReq is a pending PutAnswers call waiting in the group-commit queue.
type putReq struct {
	answers []Answer
	done    chan error
	lead    chan []*putReq
}

// SQLite is the on-disk Cache. It calls modernc.org/sqlite/lib directly with
// SQLITE_OPEN_NOMUTEX (serialized by dbMu) and reusable C-heap bind buffers,
// avoiding database/sql reflection, per-key mallocs, and SQLite's internal
// pthread mutex overhead. WAL mode, a busy timeout and INSERT OR IGNORE let
// several processes share one database.
type SQLite struct {
	dbMu sync.Mutex // guards tls, db, all stmts and C-heap scratch buffers
	tls  *libc.TLS
	db   uintptr

	model    string
	modelPtr uintptr // C string copy of model, bound with SQLITE_STATIC
	modelLen int32

	// Reusable C-heap scratch buffers bound with SQLITE_STATIC.
	keyBuf     uintptr // lookupBatch * 32 bytes
	blobBuf    uintptr
	blobBufCap int

	// Lazily prepared statements reused across calls.
	beginReadStmt        uintptr
	beginWriteStmt       uintptr
	commitStmt           uintptr
	rollbackStmt         uintptr
	putAnswerStmts       [len(answerInsertSizes)]uintptr
	putChunkStmts        [len(chunkInsertSizes)]uintptr
	getAnswerStmt        uintptr // lookupBatch size
	getChunkStmt         uintptr // lookupBatch size
	touchAnswerStmt      uintptr // lookupBatch size
	touchChunkStmt       uintptr // lookupBatch size
	scanAnswerStmt       uintptr // range cursor merge-join
	scanChunkStmt        uintptr // range cursor merge-join
	touchAnswerRangeStmt uintptr // contiguous stale range update
	touchChunkRangeStmt  uintptr // contiguous stale range update
	hasAnswerStmt        uintptr // SELECT 1 FROM answers LIMIT 1
	hasChunkStmt         uintptr // SELECT 1 FROM chunks LIMIT 1
	pageCountStmt        uintptr // PRAGMA page_count
	pageSizeStmt         uintptr // PRAGMA page_size
	pageSizeBytes        int64   // cached DB page_size (constant for DB lifetime)

	// Group-commit state for concurrent PutAnswers callers.
	putMu     sync.Mutex
	putActive bool
	putQueue  []*putReq
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
	tls := libc.NewTLS()
	c := &SQLite{tls: tls, model: model}

	cleanupOnErr := func(err error) (*SQLite, error) {
		_ = c.Close()
		return nil, err
	}

	db, err := openRawSQLite(tls, path)
	if err != nil {
		tls.Close()
		return nil, fmt.Errorf("opening cache %s: %w", path, err)
	}
	c.db = db
	sqlite3.Xsqlite3_busy_timeout(tls, db, busyTimeoutMS)

	c.modelPtr, err = libc.CString(model)
	if err != nil {
		return cleanupOnErr(fmt.Errorf("opening cache %s: %w", path, err))
	}
	c.modelLen = int32(len(model))

	// Fast path for an existing database: set per-connection pragmas and
	// check last_cleanup in a single read-only query, avoiding any write lock.
	if err := c.execNoLock(`PRAGMA synchronous=NORMAL; PRAGMA cache_size=-32768;`); err != nil {
		if !isBusy(err) {
			return cleanupOnErr(fmt.Errorf("opening cache %s: %w", path, err))
		}
	}

	t := now().Unix()
	last, found, err := c.readLastCleanupNoLock()
	if err == nil && found && t-last < int64(cleanupEvery/time.Second) {
		return c, nil
	}

	// Either the database is brand new, cleanup is due, or another process
	// holds the file while initializing WAL mode.
	freshDB := false
	if err != nil || !found {
		seedCleanup := err != nil && strings.Contains(err.Error(), "no such table")
		err = c.initNewDBNoLock(t, seedCleanup)
		for deadline := time.Now().Add(busyTimeoutMS * time.Millisecond); isBusy(err) && time.Now().Before(deadline); {
			time.Sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
			err = c.initNewDBNoLock(t, seedCleanup)
		}
		if err != nil {
			return cleanupOnErr(fmt.Errorf("opening cache %s: %w", path, err))
		}
		if seedCleanup {
			ansEmpty, err1 := c.tableEmptyNoLock(&c.hasAnswerStmt, "answers")
			chEmpty, err2 := c.tableEmptyNoLock(&c.hasChunkStmt, "chunks")
			if err1 == nil && err2 == nil && ansEmpty && chEmpty {
				freshDB = true
			}
		}
	}
	if !freshDB {
		if err := c.cleanupNoLock(); err != nil {
			return cleanupOnErr(fmt.Errorf("cleaning cache %s: %w", path, err))
		}
	}
	return c, nil
}

// cptr converts a C-heap address returned by modernc.org/libc or sqlite3 into
// an unsafe.Pointer. These addresses point to manually managed non-Go memory
// and are never moved by the Go garbage collector.
func cptr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

func openRawSQLite(tls *libc.TLS, path string) (uintptr, error) {
	pp := libc.Xmalloc(tls, types.Size_t(unsafe.Sizeof(uintptr(0))))
	if pp == 0 {
		return 0, errors.New("sqlite: out of memory")
	}
	defer libc.Xfree(tls, pp)
	*(*uintptr)(cptr(pp)) = 0

	cpath, err := libc.CString(path)
	if err != nil {
		return 0, err
	}
	defer libc.Xfree(tls, cpath)

	flags := int32(sqlite3.SQLITE_OPEN_READWRITE | sqlite3.SQLITE_OPEN_CREATE | sqlite3.SQLITE_OPEN_NOMUTEX)
	rc := sqlite3.Xsqlite3_open_v2(tls, cpath, pp, flags, 0)
	dbh := *(*uintptr)(cptr(pp))
	if rc != sqlite3.SQLITE_OK {
		msg := fmt.Sprintf("sqlite error %d", rc)
		if dbh != 0 {
			if p := sqlite3.Xsqlite3_errmsg(tls, dbh); p != 0 {
				msg = libc.GoString(p)
			}
			sqlite3.Xsqlite3_close_v2(tls, dbh)
		}
		return 0, &sqliteError{code: rc, msg: msg}
	}
	return dbh, nil
}

func (c *SQLite) Close() error {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	if c.tls == nil {
		return nil
	}
	finalize := func(sp *uintptr) {
		if *sp != 0 {
			sqlite3.Xsqlite3_finalize(c.tls, *sp)
			*sp = 0
		}
	}
	finalize(&c.beginReadStmt)
	finalize(&c.beginWriteStmt)
	finalize(&c.commitStmt)
	finalize(&c.rollbackStmt)
	finalize(&c.getAnswerStmt)
	finalize(&c.getChunkStmt)
	finalize(&c.touchAnswerStmt)
	finalize(&c.touchChunkStmt)
	finalize(&c.scanAnswerStmt)
	finalize(&c.scanChunkStmt)
	finalize(&c.touchAnswerRangeStmt)
	finalize(&c.touchChunkRangeStmt)
	finalize(&c.hasAnswerStmt)
	finalize(&c.hasChunkStmt)
	finalize(&c.pageCountStmt)
	finalize(&c.pageSizeStmt)
	for i := range c.putAnswerStmts {
		finalize(&c.putAnswerStmts[i])
	}
	for i := range c.putChunkStmts {
		finalize(&c.putChunkStmts[i])
	}
	if c.modelPtr != 0 {
		libc.Xfree(c.tls, c.modelPtr)
		c.modelPtr = 0
	}
	if c.keyBuf != 0 {
		libc.Xfree(c.tls, c.keyBuf)
		c.keyBuf = 0
	}
	if c.blobBuf != 0 {
		libc.Xfree(c.tls, c.blobBuf)
		c.blobBuf = 0
		c.blobBufCap = 0
	}
	var err error
	if c.db != 0 {
		if rc := sqlite3.Xsqlite3_close_v2(c.tls, c.db); rc != sqlite3.SQLITE_OK {
			err = c.errOf(rc)
		}
		c.db = 0
	}
	c.tls.Close()
	c.tls = nil
	return err
}

func (c *SQLite) errOf(rc int32) error {
	msg := fmt.Sprintf("sqlite error %d", rc)
	if c.db != 0 {
		if p := sqlite3.Xsqlite3_errmsg(c.tls, c.db); p != 0 {
			msg = libc.GoString(p)
		}
	}
	return &sqliteError{code: rc, msg: msg}
}

func isBusy(err error) bool {
	var se interface{ Code() int }
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

func (c *SQLite) prepareNoLock(query string) (uintptr, error) {
	cs, err := libc.CString(query)
	if err != nil {
		return 0, err
	}
	defer libc.Xfree(c.tls, cs)

	pp := libc.Xmalloc(c.tls, types.Size_t(unsafe.Sizeof(uintptr(0))))
	if pp == 0 {
		return 0, errors.New("sqlite: out of memory")
	}
	defer libc.Xfree(c.tls, pp)
	*(*uintptr)(cptr(pp)) = 0

	rc := sqlite3.Xsqlite3_prepare_v2(c.tls, c.db, cs, int32(len(query)), pp, 0)
	if rc != sqlite3.SQLITE_OK {
		return 0, c.errOf(rc)
	}
	return *(*uintptr)(cptr(pp)), nil
}

func (c *SQLite) cachedStmtNoLock(slot *uintptr, build func() string) (uintptr, error) {
	if *slot != 0 {
		return *slot, nil
	}
	s, err := c.prepareNoLock(build())
	if err != nil {
		return 0, err
	}
	*slot = s
	return s, nil
}

func (c *SQLite) execNoLock(sqlText string) error {
	cs, err := libc.CString(sqlText)
	if err != nil {
		return err
	}
	defer libc.Xfree(c.tls, cs)
	rc := sqlite3.Xsqlite3_exec(c.tls, c.db, cs, 0, 0, 0)
	if rc != sqlite3.SQLITE_OK {
		return c.errOf(rc)
	}
	return nil
}

func (c *SQLite) stepDoneNoLock(stmt uintptr) error {
	rc := sqlite3.Xsqlite3_step(c.tls, stmt)
	sqlite3.Xsqlite3_reset(c.tls, stmt)
	if rc != sqlite3.SQLITE_DONE && rc != sqlite3.SQLITE_ROW {
		return c.errOf(rc)
	}
	return nil
}

func (c *SQLite) beginReadNoLock() error {
	s, err := c.cachedStmtNoLock(&c.beginReadStmt, func() string { return `BEGIN DEFERRED` })
	if err != nil {
		return err
	}
	return c.stepDoneNoLock(s)
}

func (c *SQLite) beginWriteNoLock() error {
	s, err := c.cachedStmtNoLock(&c.beginWriteStmt, func() string { return `BEGIN IMMEDIATE` })
	if err != nil {
		return err
	}
	return c.stepDoneNoLock(s)
}

func (c *SQLite) commitNoLock() error {
	s, err := c.cachedStmtNoLock(&c.commitStmt, func() string { return `COMMIT` })
	if err != nil {
		return err
	}
	return c.stepDoneNoLock(s)
}

func (c *SQLite) rollbackNoLock() {
	s, err := c.cachedStmtNoLock(&c.rollbackStmt, func() string { return `ROLLBACK` })
	if err == nil {
		_ = c.stepDoneNoLock(s)
	}
}

func (c *SQLite) readLastCleanupNoLock() (last int64, found bool, err error) {
	stmt, err := c.prepareNoLock(`SELECT value FROM meta WHERE name = 'last_cleanup'`)
	if err != nil {
		return 0, false, err
	}
	defer sqlite3.Xsqlite3_finalize(c.tls, stmt)
	rc := sqlite3.Xsqlite3_step(c.tls, stmt)
	if rc == sqlite3.SQLITE_ROW {
		return sqlite3.Xsqlite3_column_int64(c.tls, stmt, 0), true, nil
	}
	if rc == sqlite3.SQLITE_DONE {
		return 0, false, nil
	}
	return 0, false, c.errOf(rc)
}

// initNewDBNoLock configures WAL + 16 KiB pages and creates tables inside an
// IMMEDIATE transaction so concurrent processes wait out busy_timeout.
// When seedCleanup is true (brand-new DB), it also records last_cleanup in the
// same transaction, saving an extra write transaction and fsync on cold open.
func (c *SQLite) initNewDBNoLock(t int64, seedCleanup bool) error {
	if err := c.execNoLock(`PRAGMA page_size=16384; PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA cache_size=-32768; PRAGMA temp_store=MEMORY; PRAGMA wal_autocheckpoint=2000;`); err != nil {
		return err
	}
	if err := c.beginWriteNoLock(); err != nil {
		return err
	}
	if err := c.execNoLock(schema); err != nil {
		c.rollbackNoLock()
		return err
	}
	if seedCleanup {
		stmt, err := c.prepareNoLock(`INSERT OR IGNORE INTO meta (name, value) VALUES ('last_cleanup', ?)`)
		if err != nil {
			c.rollbackNoLock()
			return err
		}
		sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, t)
		stepErr := c.stepDoneNoLock(stmt)
		sqlite3.Xsqlite3_finalize(c.tls, stmt)
		if stepErr != nil {
			c.rollbackNoLock()
			return stepErr
		}
	}
	return c.commitNoLock()
}

func (c *SQLite) cleanupNoLock() error {
	t := now().Unix()
	if err := c.beginWriteNoLock(); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			c.rollbackNoLock()
		}
	}()
	last, found, err := c.readLastCleanupNoLock()
	if err != nil {
		return err
	}
	if found && t-last < int64(cleanupEvery/time.Second) {
		return nil
	}
	stale := t - int64(staleAfter/time.Second)

	delAns, err := c.prepareNoLock(`DELETE FROM answers WHERE last_used < ? OR model != ?`)
	if err != nil {
		return err
	}
	defer sqlite3.Xsqlite3_finalize(c.tls, delAns)
	sqlite3.Xsqlite3_bind_int64(c.tls, delAns, 1, stale)
	sqlite3.Xsqlite3_bind_text(c.tls, delAns, 2, c.modelPtr, c.modelLen, 0)
	if err := c.stepDoneNoLock(delAns); err != nil {
		return err
	}

	delCh, err := c.prepareNoLock(`DELETE FROM chunks WHERE last_used < ?`)
	if err != nil {
		return err
	}
	defer sqlite3.Xsqlite3_finalize(c.tls, delCh)
	sqlite3.Xsqlite3_bind_int64(c.tls, delCh, 1, stale)
	if err := c.stepDoneNoLock(delCh); err != nil {
		return err
	}

	updMeta, err := c.prepareNoLock(`INSERT OR REPLACE INTO meta (name, value) VALUES ('last_cleanup', ?)`)
	if err != nil {
		return err
	}
	defer sqlite3.Xsqlite3_finalize(c.tls, updMeta)
	sqlite3.Xsqlite3_bind_int64(c.tls, updMeta, 1, t)
	if err := c.stepDoneNoLock(updMeta); err != nil {
		return err
	}

	if err := c.commitNoLock(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (c *SQLite) ensureBlobBufNoLock(need int) error {
	if need <= c.blobBufCap {
		return nil
	}
	newCap := max(c.blobBufCap*2, need, 64<<10)
	p := libc.Xmalloc(c.tls, types.Size_t(newCap))
	if p == 0 {
		return errors.New("sqlite: out of memory")
	}
	if c.blobBuf != 0 {
		libc.Xfree(c.tls, c.blobBuf)
	}
	c.blobBuf = p
	c.blobBufCap = newCap
	return nil
}

func (c *SQLite) ensureKeyBufNoLock() error {
	if c.keyBuf != 0 {
		return nil
	}
	c.keyBuf = libc.Xmalloc(c.tls, types.Size_t(lookupBatch*32))
	if c.keyBuf == 0 {
		return errors.New("sqlite: out of memory")
	}
	return nil
}

// copyKeysToScratch copies batch into c.keyBuf so every key can be bound with
// SQLITE_STATIC (zero mallocs, zero frees).
func (c *SQLite) copyKeysToScratchNoLock(batch []Key) {
	dst := unsafe.Slice((*Key)(cptr(c.keyBuf)), len(batch))
	copy(dst, batch)
}

// tableEmptyNoLock checks in O(1) whether table has zero rows.
func (c *SQLite) tableEmptyNoLock(slot *uintptr, table string) (bool, error) {
	stmt, err := c.cachedStmtNoLock(slot, func() string { return `SELECT 1 FROM ` + table + ` LIMIT 1` })
	if err != nil {
		return false, err
	}
	rc := sqlite3.Xsqlite3_step(c.tls, stmt)
	sqlite3.Xsqlite3_reset(c.tls, stmt)
	if rc == sqlite3.SQLITE_DONE {
		return true, nil
	}
	if rc == sqlite3.SQLITE_ROW {
		return false, nil
	}
	return false, c.errOf(rc)
}

// shouldRangeScanNoLock reports whether a single sequential cursor merge-join
// over [keys[0], keys[last]] is faster than batched IN (...) point lookups.
// Uses PRAGMA page_count * page_size as a fast upper bound on table size:
// when the total database bytes are within ~320 bytes per requested key
// (roughly <= 5-6x rows per requested key), a sequential leaf scan beats
// building ephemeral IN B-trees and doing N separate root-to-leaf probes.
func (c *SQLite) shouldRangeScanNoLock(nKeys int) bool {
	if nKeys < 32 {
		return false
	}
	if c.pageSizeBytes == 0 {
		s, err := c.cachedStmtNoLock(&c.pageSizeStmt, func() string { return `PRAGMA page_size` })
		if err != nil {
			return false
		}
		if sqlite3.Xsqlite3_step(c.tls, s) == sqlite3.SQLITE_ROW {
			c.pageSizeBytes = sqlite3.Xsqlite3_column_int64(c.tls, s, 0)
		}
		sqlite3.Xsqlite3_reset(c.tls, s)
		if c.pageSizeBytes == 0 {
			c.pageSizeBytes = 4096
		}
	}
	s, err := c.cachedStmtNoLock(&c.pageCountStmt, func() string { return `PRAGMA page_count` })
	if err != nil {
		return false
	}
	var pages int64
	if sqlite3.Xsqlite3_step(c.tls, s) == sqlite3.SQLITE_ROW {
		pages = sqlite3.Xsqlite3_column_int64(c.tls, s, 0)
	}
	sqlite3.Xsqlite3_reset(c.tls, s)
	return pages*c.pageSizeBytes <= int64(nKeys)*1200
}

// Get returns the cached probability for every key it holds, and marks
// those rows as used.
func (c *SQLite) Get(keys []Key) (map[Key]float64, error) {
	if len(keys) == 0 {
		return map[Key]float64{}, nil
	}
	t := now().Unix()
	cutoff := t - int64(touchAfter/time.Second)

	c.dbMu.Lock()
	defer c.dbMu.Unlock()

	if err := c.beginReadNoLock(); err != nil {
		return nil, err
	}
	empty, err := c.tableEmptyNoLock(&c.hasAnswerStmt, "answers")
	if err != nil {
		c.rollbackNoLock()
		return nil, err
	}
	if empty {
		c.rollbackNoLock()
		return map[Key]float64{}, nil
	}

	if err := c.ensureKeyBufNoLock(); err != nil {
		c.rollbackNoLock()
		return nil, err
	}

	sorted := sortAndCompactKeys(keys)
	out := make(map[Key]float64, len(sorted))
	var stale []Key
	contiguousStale := false

	if c.shouldRangeScanNoLock(len(sorted)) {
		contiguousStale = true
		stmt, err := c.cachedStmtNoLock(&c.scanAnswerStmt, func() string {
			return `SELECT key, noul, last_used < ? FROM answers WHERE key >= ? AND key <= ?`
		})
		if err != nil {
			c.rollbackNoLock()
			return nil, err
		}
		keyDst := unsafe.Slice((*Key)(cptr(c.keyBuf)), 2)
		keyDst[0] = sorted[0]
		keyDst[1] = sorted[len(sorted)-1]
		sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, cutoff)
		sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 2, c.keyBuf, 32, 0)
		sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 3, c.keyBuf+32, 32, 0)

		ki := 0
		nSorted := len(sorted)
		seenStale := false
		for {
			rc := sqlite3.Xsqlite3_step(c.tls, stmt)
			if rc == sqlite3.SQLITE_ROW {
				kp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 0)
				if kp == 0 {
					continue
				}
				k := *(*Key)(cptr(kp))
				// Fast path: exact match on full-cache hits avoids compareKeys.
				if ki < nSorted && sorted[ki] != k {
					for ki < nSorted && compareKeys(sorted[ki], k) < 0 {
						ki++
					}
				}
				if ki < nSorted && sorted[ki] == k {
					out[k] = sqlite3.Xsqlite3_column_double(c.tls, stmt, 1)
					if sqlite3.Xsqlite3_column_int64(c.tls, stmt, 2) != 0 {
						stale = append(stale, k)
						seenStale = true
					} else if seenStale {
						contiguousStale = false
					}
					ki++
				} else if seenStale {
					// Unrequested row inside stale range: cannot use range UPDATE.
					contiguousStale = false
				}
				continue
			}
			sqlite3.Xsqlite3_reset(c.tls, stmt)
			if rc != sqlite3.SQLITE_DONE {
				c.rollbackNoLock()
				return nil, c.errOf(rc)
			}
			break
		}
	} else {
		query := func(n int) string {
			return `SELECT key, noul, last_used < ? FROM answers WHERE key IN (` + params(n) + `)`
		}
		err = c.forKeyBatchesNoLock(sorted, &c.getAnswerStmt, query, func(stmt uintptr, batch []Key) error {
			sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, cutoff)
			for i := range batch {
				sqlite3.Xsqlite3_bind_blob(c.tls, stmt, int32(i+2), c.keyBuf+uintptr(i*32), 32, 0)
			}
			for {
				rc := sqlite3.Xsqlite3_step(c.tls, stmt)
				if rc == sqlite3.SQLITE_ROW {
					kp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 0)
					if kp == 0 {
						continue
					}
					k := *(*Key)(cptr(kp))
					out[k] = sqlite3.Xsqlite3_column_double(c.tls, stmt, 1)
					if sqlite3.Xsqlite3_column_int64(c.tls, stmt, 2) != 0 {
						stale = append(stale, k)
					}
					continue
				}
				sqlite3.Xsqlite3_reset(c.tls, stmt)
				if rc == sqlite3.SQLITE_DONE {
					return nil
				}
				return c.errOf(rc)
			}
		})
		if err != nil {
			c.rollbackNoLock()
			return nil, err
		}
	}
	c.rollbackNoLock()
	if len(stale) > 0 {
		if contiguousStale && len(stale) >= 32 {
			if err := c.touchRangeNoLock("answers", &c.touchAnswerRangeStmt, stale[0], stale[len(stale)-1], cutoff, t); err != nil {
				return nil, err
			}
		} else {
			if err := c.touchNoLock("answers", &c.touchAnswerStmt, stale, t); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// GetInto fills vals[i] and found[i] for every cached key in keys without
// building an intermediate map[Key]float64, and marks found rows as used.
func (c *SQLite) GetInto(keys []Key, vals []float64, found []bool) error {
	if len(keys) == 0 {
		return nil
	}
	t := now().Unix()
	cutoff := t - int64(touchAfter/time.Second)

	c.dbMu.Lock()
	defer c.dbMu.Unlock()

	if err := c.beginReadNoLock(); err != nil {
		return err
	}
	empty, err := c.tableEmptyNoLock(&c.hasAnswerStmt, "answers")
	if err != nil {
		c.rollbackNoLock()
		return err
	}
	if empty {
		c.rollbackNoLock()
		return nil
	}

	if err := c.ensureKeyBufNoLock(); err != nil {
		c.rollbackNoLock()
		return err
	}

	idx := sortKeyIndices(keys)
	uniq := make([]Key, 0, len(keys))
	type span struct{ lo, hi int }
	spans := make([]span, 0, len(keys))
	for i := 0; i < len(idx); {
		k := keys[idx[i]]
		j := i + 1
		for j < len(idx) && keys[idx[j]] == k {
			j++
		}
		uniq = append(uniq, k)
		spans = append(spans, span{i, j})
		i = j
	}

	var stale []Key
	contiguousStale := false

	if c.shouldRangeScanNoLock(len(uniq)) {
		contiguousStale = true
		stmt, err := c.cachedStmtNoLock(&c.scanAnswerStmt, func() string {
			return `SELECT key, noul, last_used < ? FROM answers WHERE key >= ? AND key <= ?`
		})
		if err != nil {
			c.rollbackNoLock()
			return err
		}
		keyDst := unsafe.Slice((*Key)(cptr(c.keyBuf)), 2)
		keyDst[0] = uniq[0]
		keyDst[1] = uniq[len(uniq)-1]
		sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, cutoff)
		sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 2, c.keyBuf, 32, 0)
		sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 3, c.keyBuf+32, 32, 0)

		ki := 0
		nUniq := len(uniq)
		seenStale := false
		for {
			rc := sqlite3.Xsqlite3_step(c.tls, stmt)
			if rc == sqlite3.SQLITE_ROW {
				kp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 0)
				if kp == 0 {
					continue
				}
				k := *(*Key)(cptr(kp))
				if ki < nUniq && uniq[ki] != k {
					for ki < nUniq && compareKeys(uniq[ki], k) < 0 {
						ki++
					}
				}
				if ki < nUniq && uniq[ki] == k {
					v := sqlite3.Xsqlite3_column_double(c.tls, stmt, 1)
					sp := spans[ki]
					for p := sp.lo; p < sp.hi; p++ {
						orig := idx[p]
						vals[orig] = v
						found[orig] = true
					}
					if sqlite3.Xsqlite3_column_int64(c.tls, stmt, 2) != 0 {
						stale = append(stale, k)
						seenStale = true
					} else if seenStale {
						contiguousStale = false
					}
					ki++
				} else if seenStale {
					contiguousStale = false
				}
				continue
			}
			sqlite3.Xsqlite3_reset(c.tls, stmt)
			if rc != sqlite3.SQLITE_DONE {
				c.rollbackNoLock()
				return c.errOf(rc)
			}
			break
		}
	} else {
		query := func(n int) string {
			return `SELECT key, noul, last_used < ? FROM answers WHERE key IN (` + params(n) + `)`
		}
		batchBase := 0
		err = c.forKeyBatchesNoLock(uniq, &c.getAnswerStmt, query, func(stmt uintptr, batch []Key) error {
			sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, cutoff)
			for i := range batch {
				sqlite3.Xsqlite3_bind_blob(c.tls, stmt, int32(i+2), c.keyBuf+uintptr(i*32), 32, 0)
			}
			for {
				rc := sqlite3.Xsqlite3_step(c.tls, stmt)
				if rc == sqlite3.SQLITE_ROW {
					kp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 0)
					if kp == 0 {
						continue
					}
					k := *(*Key)(cptr(kp))
					v := sqlite3.Xsqlite3_column_double(c.tls, stmt, 1)
					if pos, ok := slices.BinarySearchFunc(batch, k, compareKeys); ok {
						sp := spans[batchBase+pos]
						for p := sp.lo; p < sp.hi; p++ {
							orig := idx[p]
							vals[orig] = v
							found[orig] = true
						}
					}
					if sqlite3.Xsqlite3_column_int64(c.tls, stmt, 2) != 0 {
						stale = append(stale, k)
					}
					continue
				}
				sqlite3.Xsqlite3_reset(c.tls, stmt)
				if rc == sqlite3.SQLITE_DONE {
					batchBase += len(batch)
					return nil
				}
				return c.errOf(rc)
			}
		})
		if err != nil {
			c.rollbackNoLock()
			return err
		}
	}
	c.rollbackNoLock()
	if len(stale) > 0 {
		if contiguousStale && len(stale) >= 32 {
			if err := c.touchRangeNoLock("answers", &c.touchAnswerRangeStmt, stale[0], stale[len(stale)-1], cutoff, t); err != nil {
				return err
			}
		} else {
			if err := c.touchNoLock("answers", &c.touchAnswerStmt, stale, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetChunks returns the stored chunk layout for every key it holds, and
// marks those rows as used.
func (c *SQLite) GetChunks(keys []Key) (map[Key][]byte, error) {
	if len(keys) == 0 {
		return map[Key][]byte{}, nil
	}
	t := now().Unix()
	cutoff := t - int64(touchAfter/time.Second)

	c.dbMu.Lock()
	defer c.dbMu.Unlock()

	if err := c.beginReadNoLock(); err != nil {
		return nil, err
	}
	empty, err := c.tableEmptyNoLock(&c.hasChunkStmt, "chunks")
	if err != nil {
		c.rollbackNoLock()
		return nil, err
	}
	if empty {
		c.rollbackNoLock()
		return map[Key][]byte{}, nil
	}

	if err := c.ensureKeyBufNoLock(); err != nil {
		c.rollbackNoLock()
		return nil, err
	}

	sorted := sortAndCompactKeys(keys)
	out := make(map[Key][]byte, len(sorted))
	var stale []Key
	contiguousStale := false

	const slabSize = 64 << 10
	var slab []byte
	slabOff := 0
	readBlob := func(stmt uintptr) []byte {
		ln := int(sqlite3.Xsqlite3_column_bytes(c.tls, stmt, 1))
		if ln == 0 {
			return []byte{}
		}
		lp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 1)
		var val []byte
		if ln <= 4096 {
			if len(slab)-slabOff < ln {
				slab = make([]byte, slabSize)
				slabOff = 0
			}
			val = slab[slabOff : slabOff+ln : slabOff+ln]
			slabOff += ln
		} else {
			val = make([]byte, ln)
		}
		copy(val, unsafe.Slice((*byte)(cptr(lp)), ln))
		return val
	}

	if c.shouldRangeScanNoLock(len(sorted)) {
		contiguousStale = true
		stmt, err := c.cachedStmtNoLock(&c.scanChunkStmt, func() string {
			return `SELECT key, layout, last_used < ? FROM chunks WHERE key >= ? AND key <= ?`
		})
		if err != nil {
			c.rollbackNoLock()
			return nil, err
		}
		keyDst := unsafe.Slice((*Key)(cptr(c.keyBuf)), 2)
		keyDst[0] = sorted[0]
		keyDst[1] = sorted[len(sorted)-1]
		sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, cutoff)
		sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 2, c.keyBuf, 32, 0)
		sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 3, c.keyBuf+32, 32, 0)

		ki := 0
		nSorted := len(sorted)
		seenStale := false
		for {
			rc := sqlite3.Xsqlite3_step(c.tls, stmt)
			if rc == sqlite3.SQLITE_ROW {
				kp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 0)
				if kp == 0 {
					continue
				}
				k := *(*Key)(cptr(kp))
				if ki < nSorted && sorted[ki] != k {
					for ki < nSorted && compareKeys(sorted[ki], k) < 0 {
						ki++
					}
				}
				if ki < nSorted && sorted[ki] == k {
					out[k] = readBlob(stmt)
					if sqlite3.Xsqlite3_column_int64(c.tls, stmt, 2) != 0 {
						stale = append(stale, k)
						seenStale = true
					} else if seenStale {
						contiguousStale = false
					}
					ki++
				} else if seenStale {
					contiguousStale = false
				}
				continue
			}
			sqlite3.Xsqlite3_reset(c.tls, stmt)
			if rc != sqlite3.SQLITE_DONE {
				c.rollbackNoLock()
				return nil, c.errOf(rc)
			}
			break
		}
	} else {
		query := func(n int) string {
			return `SELECT key, layout, last_used < ? FROM chunks WHERE key IN (` + params(n) + `)`
		}
		err = c.forKeyBatchesNoLock(sorted, &c.getChunkStmt, query, func(stmt uintptr, batch []Key) error {
			sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, cutoff)
			for i := range batch {
				sqlite3.Xsqlite3_bind_blob(c.tls, stmt, int32(i+2), c.keyBuf+uintptr(i*32), 32, 0)
			}
			for {
				rc := sqlite3.Xsqlite3_step(c.tls, stmt)
				if rc == sqlite3.SQLITE_ROW {
					kp := sqlite3.Xsqlite3_column_blob(c.tls, stmt, 0)
					if kp == 0 {
						continue
					}
					k := *(*Key)(cptr(kp))
					out[k] = readBlob(stmt)
					if sqlite3.Xsqlite3_column_int64(c.tls, stmt, 2) != 0 {
						stale = append(stale, k)
					}
					continue
				}
				sqlite3.Xsqlite3_reset(c.tls, stmt)
				if rc == sqlite3.SQLITE_DONE {
					return nil
				}
				return c.errOf(rc)
			}
		})
		if err != nil {
			c.rollbackNoLock()
			return nil, err
		}
	}
	c.rollbackNoLock()
	if len(stale) > 0 {
		if contiguousStale && len(stale) >= 32 {
			if err := c.touchRangeNoLock("chunks", &c.touchChunkRangeStmt, stale[0], stale[len(stale)-1], cutoff, t); err != nil {
				return nil, err
			}
		} else {
			if err := c.touchNoLock("chunks", &c.touchChunkStmt, stale, t); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (c *SQLite) touchRangeNoLock(table string, slot *uintptr, lo, hi Key, cutoff, t int64) error {
	if err := c.ensureKeyBufNoLock(); err != nil {
		return err
	}
	if err := c.beginWriteNoLock(); err != nil {
		return err
	}
	stmt, err := c.cachedStmtNoLock(slot, func() string {
		return `UPDATE ` + table + ` SET last_used = ? WHERE key >= ? AND key <= ? AND last_used < ?`
	})
	if err != nil {
		c.rollbackNoLock()
		return err
	}
	keyDst := unsafe.Slice((*Key)(cptr(c.keyBuf)), 2)
	keyDst[0] = lo
	keyDst[1] = hi
	sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, t)
	sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 2, c.keyBuf, 32, 0)
	sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 3, c.keyBuf+32, 32, 0)
	sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 4, cutoff)
	if err := c.stepDoneNoLock(stmt); err != nil {
		c.rollbackNoLock()
		return err
	}
	return c.commitNoLock()
}

func (c *SQLite) touchNoLock(table string, fullSlot *uintptr, keys []Key, t int64) error {
	if err := c.beginWriteNoLock(); err != nil {
		return err
	}
	query := func(n int) string { return `UPDATE ` + table + ` SET last_used = ? WHERE key IN (` + params(n) + `)` }
	err := c.forKeyBatchesNoLock(keys, fullSlot, query, func(stmt uintptr, batch []Key) error {
		sqlite3.Xsqlite3_bind_int64(c.tls, stmt, 1, t)
		for i := range batch {
			sqlite3.Xsqlite3_bind_blob(c.tls, stmt, int32(i+2), c.keyBuf+uintptr(i*32), 32, 0)
		}
		return c.stepDoneNoLock(stmt)
	})
	if err != nil {
		c.rollbackNoLock()
		return err
	}
	return c.commitNoLock()
}

func (c *SQLite) forKeyBatchesNoLock(keys []Key, fullSlot *uintptr, query func(n int) string, fn func(stmt uintptr, batch []Key) error) error {
	if err := c.ensureKeyBufNoLock(); err != nil {
		return err
	}
	var fullStmt uintptr
	if len(keys) >= lookupBatch && fullSlot != nil {
		var err error
		if fullStmt, err = c.cachedStmtNoLock(fullSlot, func() string { return query(lookupBatch) }); err != nil {
			return err
		}
	}
	for batch := range slices.Chunk(keys, lookupBatch) {
		c.copyKeysToScratchNoLock(batch)
		if len(batch) == lookupBatch && fullStmt != 0 {
			if err := fn(fullStmt, batch); err != nil {
				return err
			}
			continue
		}
		stmt, err := c.prepareNoLock(query(len(batch)))
		if err != nil {
			return err
		}
		err = fn(stmt, batch)
		sqlite3.Xsqlite3_finalize(c.tls, stmt)
		if err != nil {
			return err
		}
	}
	return nil
}

func compareKeys(a, b Key) int {
	if x, y := binary.BigEndian.Uint64(a[0:8]), binary.BigEndian.Uint64(b[0:8]); x != y {
		return cmp.Compare(x, y)
	}
	if x, y := binary.BigEndian.Uint64(a[8:16]), binary.BigEndian.Uint64(b[8:16]); x != y {
		return cmp.Compare(x, y)
	}
	if x, y := binary.BigEndian.Uint64(a[16:24]), binary.BigEndian.Uint64(b[16:24]); x != y {
		return cmp.Compare(x, y)
	}
	return cmp.Compare(binary.BigEndian.Uint64(a[24:32]), binary.BigEndian.Uint64(b[24:32]))
}

// sortAndCompactKeys returns a sorted, deduplicated copy of keys using a
// 256-bucket MSD radix pass on byte 0 for larger slices.
func sortAndCompactKeys(keys []Key) []Key {
	if len(keys) < 256 {
		out := slices.Clone(keys)
		slices.SortFunc(out, compareKeys)
		return slices.Compact(out)
	}
	var counts [256]int
	for i := range keys {
		counts[keys[i][0]]++
	}
	var offsets [256]int
	sum := 0
	for b, n := range counts {
		offsets[b] = sum
		sum += n
	}
	out := make([]Key, len(keys))
	pos := offsets
	for i := range keys {
		b := keys[i][0]
		out[pos[b]] = keys[i]
		pos[b]++
	}
	for b, n := range counts {
		if n > 1 {
			lo := offsets[b]
			slices.SortFunc(out[lo:lo+n], compareKeys)
		}
	}
	return slices.Compact(out)
}

// sortKeyIndices returns indices 0..len(keys)-1 ordered by keys[i] ascending.
func sortKeyIndices(keys []Key) []int {
	n := len(keys)
	if n < 256 {
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i
		}
		slices.SortFunc(idx, func(a, b int) int { return compareKeys(keys[a], keys[b]) })
		return idx
	}
	var counts [256]int
	for i := range keys {
		counts[keys[i][0]]++
	}
	var offsets [256]int
	sum := 0
	for b, c := range counts {
		offsets[b] = sum
		sum += c
	}
	idx := make([]int, n)
	pos := offsets
	for i := range keys {
		b := keys[i][0]
		idx[pos[b]] = i
		pos[b]++
	}
	for b, c := range counts {
		if c > 1 {
			lo := offsets[b]
			slices.SortFunc(idx[lo:lo+c], func(a, b int) int { return compareKeys(keys[a], keys[b]) })
		}
	}
	return idx
}

// sortAnswersByKey returns answers sorted by Key ascending, preserving order
// for equal keys.
func sortAnswersByKey(answers []Answer) []Answer {
	if len(answers) < 256 {
		slices.SortStableFunc(answers, func(a, b Answer) int { return compareKeys(a.Key, b.Key) })
		return answers
	}
	var counts [256]int
	for i := range answers {
		counts[answers[i].Key[0]]++
	}
	var offsets [256]int
	sum := 0
	for b, c := range counts {
		offsets[b] = sum
		sum += c
	}
	out := make([]Answer, len(answers))
	pos := offsets
	for i := range answers {
		b := answers[i].Key[0]
		out[pos[b]] = answers[i]
		pos[b]++
	}
	for b, c := range counts {
		if c > 1 {
			lo := offsets[b]
			slices.SortStableFunc(out[lo:lo+c], func(a, b Answer) int { return compareKeys(a.Key, b.Key) })
		}
	}
	return out
}

// params is n comma-separated placeholders.
func params(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func multiRowPlaceholders(rows, cols int) string {
	row := "(" + params(cols) + ")"
	var b strings.Builder
	b.Grow(rows*(len(row)+1) - 1)
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(row)
	}
	return b.String()
}

// PutAnswers saves answers in one transaction, coalescing concurrent callers
// into a single group-commit transaction when multiple workers write at once.
// Every caller still waits until its own answers are durably committed.
// An existing answer for a key is kept, and so is its last_used: the key
// already pins the exact model, question and code. A key repeated within
// answers is likewise written once.
func (c *SQLite) PutAnswers(answers []Answer) error {
	if len(answers) == 0 {
		return nil
	}
	c.putMu.Lock()
	if !c.putActive {
		c.putActive = true
		c.putMu.Unlock()
		err := c.commitAnswers(answers)
		c.finishPut()
		return err
	}
	req := &putReq{
		answers: answers,
		done:    make(chan error, 1),
		lead:    make(chan []*putReq, 1),
	}
	c.putQueue = append(c.putQueue, req)
	c.putMu.Unlock()

	select {
	case err := <-req.done:
		return err
	case batch := <-req.lead:
		err := c.commitAnswerBatches(batch)
		for _, r := range batch[1:] {
			r.done <- err
		}
		c.finishPut()
		return err
	}
}

func (c *SQLite) finishPut() {
	c.putMu.Lock()
	if len(c.putQueue) == 0 {
		c.putActive = false
		c.putMu.Unlock()
		return
	}
	batch := c.putQueue
	c.putQueue = nil
	c.putMu.Unlock()
	batch[0].lead <- batch
}

func (c *SQLite) commitAnswerBatches(batch []*putReq) error {
	if len(batch) == 1 {
		return c.commitAnswers(batch[0].answers)
	}
	total := 0
	for _, r := range batch {
		total += len(r.answers)
	}
	merged := make([]Answer, 0, total)
	for _, r := range batch {
		merged = append(merged, r.answers...)
	}
	merged = sortAnswersByKey(merged)
	return c.commitAnswers(merged)
}

func (c *SQLite) commitAnswers(answers []Answer) error {
	t := now().Unix()
	c.dbMu.Lock()
	defer c.dbMu.Unlock()

	if err := c.ensureKeyBufNoLock(); err != nil {
		return err
	}
	if err := c.beginWriteNoLock(); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			c.rollbackNoLock()
		}
	}()

	keyDst := unsafe.Slice((*Key)(cptr(c.keyBuf)), lookupBatch)
	rem := answers
	for si, size := range answerInsertSizes {
		if len(rem) < size {
			continue
		}
		stmt, err := c.cachedStmtNoLock(&c.putAnswerStmts[si], func() string {
			return `INSERT OR IGNORE INTO answers (key, noul, model, last_used) VALUES ` + multiRowPlaceholders(size, 4)
		})
		if err != nil {
			return err
		}
		for len(rem) >= size {
			for i := range size {
				keyDst[i] = rem[i].Key
				base := int32(i * 4)
				sqlite3.Xsqlite3_bind_blob(c.tls, stmt, base+1, c.keyBuf+uintptr(i*32), 32, 0)
				sqlite3.Xsqlite3_bind_double(c.tls, stmt, base+2, rem[i].Noul)
				sqlite3.Xsqlite3_bind_text(c.tls, stmt, base+3, c.modelPtr, c.modelLen, 0)
				sqlite3.Xsqlite3_bind_int64(c.tls, stmt, base+4, t)
			}
			if err := c.stepDoneNoLock(stmt); err != nil {
				return err
			}
			rem = rem[size:]
		}
	}
	if err := c.commitNoLock(); err != nil {
		return err
	}
	committed = true
	return nil
}

// PutChunks saves chunk layouts in one transaction. As with answers, an
// existing layout for a key is kept. Layouts are inserted in key order,
// which fills the table's pages one after another instead of at random.
func (c *SQLite) PutChunks(layouts map[Key][]byte) error {
	if len(layouts) == 0 {
		return nil
	}
	keys := make([]Key, 0, len(layouts))
	for k := range layouts {
		keys = append(keys, k)
	}
	keys = sortAndCompactKeys(keys)
	t := now().Unix()

	c.dbMu.Lock()
	defer c.dbMu.Unlock()

	if err := c.ensureKeyBufNoLock(); err != nil {
		return err
	}
	if err := c.beginWriteNoLock(); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			c.rollbackNoLock()
		}
	}()

	keyDst := unsafe.Slice((*Key)(cptr(c.keyBuf)), lookupBatch)
	rem := keys
	for si, size := range chunkInsertSizes {
		if len(rem) < size {
			continue
		}
		stmt, err := c.cachedStmtNoLock(&c.putChunkStmts[si], func() string {
			return `INSERT OR IGNORE INTO chunks (key, layout, last_used) VALUES ` + multiRowPlaceholders(size, 3)
		})
		if err != nil {
			return err
		}
		for len(rem) >= size {
			totalBytes := 0
			for i := range size {
				totalBytes += len(layouts[rem[i]])
			}
			if totalBytes > 0 {
				if err := c.ensureBlobBufNoLock(totalBytes); err != nil {
					return err
				}
			}
			var blobSlice []byte
			if totalBytes > 0 {
				blobSlice = unsafe.Slice((*byte)(cptr(c.blobBuf)), totalBytes)
			}
			off := 0
			for i := range size {
				k := rem[i]
				v := layouts[k]
				keyDst[i] = k
				base := int32(i * 3)
				sqlite3.Xsqlite3_bind_blob(c.tls, stmt, base+1, c.keyBuf+uintptr(i*32), 32, 0)
				if len(v) == 0 {
					sqlite3.Xsqlite3_bind_blob(c.tls, stmt, base+2, c.keyBuf, 0, 0)
				} else {
					copy(blobSlice[off:], v)
					sqlite3.Xsqlite3_bind_blob(c.tls, stmt, base+2, c.blobBuf+uintptr(off), int32(len(v)), 0)
					off += len(v)
				}
				sqlite3.Xsqlite3_bind_int64(c.tls, stmt, base+3, t)
			}
			if err := c.stepDoneNoLock(stmt); err != nil {
				return err
			}
			rem = rem[size:]
		}
	}
	if err := c.commitNoLock(); err != nil {
		return err
	}
	committed = true
	return nil
}

// execSQL runs raw SQL statements on the underlying SQLite handle (used by tests).
func (c *SQLite) execSQL(sqlText string) error {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	return c.execNoLock(sqlText)
}

// queryLastUsed reads last_used for key k from answers (used by tests).
func (c *SQLite) queryLastUsed(k Key) (int64, error) {
	c.dbMu.Lock()
	defer c.dbMu.Unlock()
	if err := c.ensureKeyBufNoLock(); err != nil {
		return 0, err
	}
	stmt, err := c.prepareNoLock(`SELECT last_used FROM answers WHERE key = ?`)
	if err != nil {
		return 0, err
	}
	defer sqlite3.Xsqlite3_finalize(c.tls, stmt)
	*(*Key)(cptr(c.keyBuf)) = k
	sqlite3.Xsqlite3_bind_blob(c.tls, stmt, 1, c.keyBuf, 32, 0)
	rc := sqlite3.Xsqlite3_step(c.tls, stmt)
	if rc == sqlite3.SQLITE_ROW {
		return sqlite3.Xsqlite3_column_int64(c.tls, stmt, 0), nil
	}
	return 0, c.errOf(rc)
}
