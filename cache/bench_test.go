package cache

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A state about the size of a typical chunk as Jev is shown it.
var benchState = []byte(`{"path":"src/payments/retry.ts","code":"` + strings.Repeat("retry(payment, attempt + 1); ", 50) + `"}`)

func BenchmarkKeyOf(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		KeyOf("jev-1.13.0", "The `code` retries failed payments when the card is declined", benchState)
	}
}

func BenchmarkKeyOfParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			KeyOf("jev-1.13.0", "The `code` retries failed payments when the card is declined", benchState)
		}
	})
}

// filled opens a cache holding n answers and n layouts for keys 0…n-1.
func filled(b *testing.B, n int) (*SQLite, []Key) {
	b.Helper()
	c, err := Open(filepath.Join(b.TempDir(), "repo.db"), "m")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { c.Close() })
	keys := make([]Key, n)
	layouts := make(map[Key][]byte, n)
	for i := range keys {
		keys[i] = key(i)
		layouts[keys[i]] = []byte(strings.Repeat("x", 200))
	}
	tx, err := c.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i, k := range keys {
		if _, err := tx.Exec(`INSERT INTO answers (key, noul, model, last_used) VALUES (?, ?, 'm', ?)`, k[:], float64(i)/float64(n), now().Unix()); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if err := c.PutChunks(layouts); err != nil {
		b.Fatal(err)
	}
	return c, keys
}

func BenchmarkGet(b *testing.B) {
	for _, n := range []int{1000, 20000} {
		b.Run(fmt.Sprintf("hits-%d", n), func(b *testing.B) {
			c, keys := filled(b, n)
			b.ReportAllocs()
			for b.Loop() {
				if got, err := c.Get(keys); err != nil || len(got) != n {
					b.Fatal(len(got), err)
				}
			}
		})
		b.Run(fmt.Sprintf("misses-%d", n), func(b *testing.B) {
			c, _ := filled(b, 0)
			keys := make([]Key, n)
			for i := range keys {
				keys[i] = key(i)
			}
			b.ReportAllocs()
			for b.Loop() {
				if got, err := c.Get(keys); err != nil || len(got) != 0 {
					b.Fatal(len(got), err)
				}
			}
		})
	}
}

// BenchmarkGetTouch measures a lookup that refreshes last_used on every
// row, as the first search of the day does.
func BenchmarkGetTouch(b *testing.B) {
	const n = 20000
	c, keys := filled(b, n)
	defer func() { now = time.Now }()
	day := 0
	b.ReportAllocs()
	for b.Loop() {
		day += 2
		t := time.Now().Add(time.Duration(day) * 24 * time.Hour)
		now = func() time.Time { return t }
		if got, err := c.Get(keys); err != nil || len(got) != n {
			b.Fatal(len(got), err)
		}
	}
}

func BenchmarkGetChunks(b *testing.B) {
	const n = 5000
	c, keys := filled(b, n)
	b.ReportAllocs()
	for b.Loop() {
		if got, err := c.GetChunks(keys); err != nil || len(got) != n {
			b.Fatal(len(got), err)
		}
	}
}

// callAnswers is how many answers one Jev call brings back in a typical
// search: a pack of about 8 chunks, asked 3 questions each.
const callAnswers = 8 * 3

// spreadKey makes a distinct key for each n, spread over the key space
// like a real sha256 key but without the cost of hashing.
func spreadKey(n uint64) (k Key) {
	n *= 4
	for i := range 4 {
		n += 0x9e3779b97f4a7c15 // splitmix64
		z := n
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		binary.LittleEndian.PutUint64(k[i*8:], z^(z>>31))
	}
	return k
}

// BenchmarkPutAnswers saves the answers from one Jev call, from one
// goroutine and from as many as a search's workers, and reports the cost
// per answer. Every answer is new. The cost depends on how many rows the
// table already holds, since each new key most likely lands on a page of
// its own: rows-0 is a repo's first search, rows-200000 a well-used cache.
func BenchmarkPutAnswers(b *testing.B) {
	call := func(next *atomic.Uint64) []Answer {
		answers := make([]Answer, callAnswers)
		for i := range answers {
			answers[i] = Answer{Key: spreadKey(next.Add(1)), Noul: 0.5}
		}
		return answers
	}
	// prefilled opens a cache already holding rows answers, none of them
	// for the keys call makes.
	prefilled := func(b *testing.B, rows int) *SQLite {
		c, _ := filled(b, 0)
		tx, err := c.db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		for i := range rows {
			k := spreadKey(1<<40 + uint64(i))
			if _, err := tx.Exec(`INSERT INTO answers (key, noul, model, last_used) VALUES (?, 0.5, 'm', ?)`, k[:], now().Unix()); err != nil {
				b.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
		if _, err := c.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			b.Fatal(err)
		}
		return c
	}
	perAnswer := func(b *testing.B) {
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*callAnswers), "ns/answer")
	}
	for _, rows := range []int{0, 200_000} {
		b.Run(fmt.Sprintf("serial/rows-%d", rows), func(b *testing.B) {
			c := prefilled(b, rows)
			var next atomic.Uint64
			b.ReportAllocs()
			for b.Loop() {
				if err := c.PutAnswers(call(&next)); err != nil {
					b.Fatal(err)
				}
			}
			perAnswer(b)
		})
		b.Run(fmt.Sprintf("parallel/rows-%d", rows), func(b *testing.B) {
			c := prefilled(b, rows)
			var next atomic.Uint64
			b.SetParallelism(max(1, 128/runtime.GOMAXPROCS(0))) // about search.DefaultWorkers goroutines
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := c.PutAnswers(call(&next)); err != nil {
						b.Error(err)
						return
					}
				}
			})
			perAnswer(b)
		})
	}
}

func BenchmarkOpen(b *testing.B) {
	path := filepath.Join(b.TempDir(), "repo.db")
	c, err := Open(path, "m")
	if err != nil {
		b.Fatal(err)
	}
	c.Close()
	b.ReportAllocs()
	for b.Loop() {
		c, err := Open(path, "m")
		if err != nil {
			b.Fatal(err)
		}
		c.Close()
	}
}

// BenchmarkPutChunks saves the layouts of a 5000-file repo searched for
// the first time.
func BenchmarkPutChunks(b *testing.B) {
	c, _ := filled(b, 0)
	const n = 5000
	layout := []byte(strings.Repeat("x", 200))
	next := 0
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		layouts := make(map[Key][]byte, n)
		for range n {
			next++
			layouts[key(next)] = layout
		}
		b.StartTimer()
		if err := c.PutChunks(layouts); err != nil {
			b.Fatal(err)
		}
	}
}
