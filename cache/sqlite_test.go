package cache

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func key(i int) Key { return KeyOf("m", strconv.Itoa(i), nil) }

func open(t *testing.T, path, model string) *SQLite {
	t.Helper()
	c, err := Open(path, model)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAnswersPersistAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "repo.db")
	c := open(t, path, "jev-1.13.0")
	if err := c.Put(key(1), 0.94); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key(1), 0.10); err != nil {
		t.Fatal(err)
	}
	c.Close()

	c = open(t, path, "jev-1.13.0")
	defer c.Close()
	got, err := c.Get([]Key{key(1), key(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[key(1)] != 0.94 {
		t.Fatalf("got %v", got)
	}
}

func TestGetLooksUpManyKeysAtOnce(t *testing.T) {
	c := open(t, filepath.Join(t.TempDir(), "repo.db"), "m")
	defer c.Close()
	var keys []Key
	for i := range 5000 {
		keys = append(keys, key(i))
		if i%2 == 0 {
			if err := c.Put(key(i), float64(i)/5000); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := c.Get(keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2500 || got[key(4998)] != 4998.0/5000 {
		t.Fatalf("got %d hits", len(got))
	}
}

func TestCleanupDropsStaleAndOtherModelRowsOnceADay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.db")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) { now = func() time.Time { return t0.Add(d) } }
	defer func() { now = time.Now }()
	day := 24 * time.Hour

	at(0)
	c := open(t, path, "A")
	c.Put(key(1), 0.1)
	c.Put(key(2), 0.2)
	c.Close()

	at(20 * day) // using key 2 keeps it fresh
	c = open(t, path, "A")
	c.Get([]Key{key(2)})
	c.Close()

	at(31 * day)
	c = open(t, path, "A")
	got, _ := c.Get([]Key{key(1), key(2)})
	c.Close()
	if _, ok := got[key(1)]; ok || len(got) != 1 {
		t.Fatalf("after 31 days unused key 1 should be gone, got %v", got)
	}

	at(31*day + time.Hour) // cleanup already ran today
	c = open(t, path, "B")
	got, _ = c.Get([]Key{key(2)})
	c.Close()
	if len(got) != 1 {
		t.Fatal("cleanup should run at most once a day")
	}

	at(32*day + 2*time.Hour)
	c = open(t, path, "B")
	got, _ = c.Get([]Key{key(2)})
	c.Close()
	if len(got) != 0 {
		t.Fatal("rows from other model versions should be dropped")
	}
}

func TestPathAndClear(t *testing.T) {
	dir := t.TempDir()
	a, b := Path(dir, "/src/one"), Path(dir, "/src/two")
	if a == b || a != Path(dir, "/src/one") || filepath.Dir(a) != dir {
		t.Fatalf("paths %s %s", a, b)
	}
	c := open(t, a, "m")
	c.Put(key(1), 0.5)
	c.Close()
	if err := Clear(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("database still there: %v", err)
	}
	if err := Clear(a); err != nil {
		t.Fatalf("clearing a missing cache is fine: %v", err)
	}
}

// TestHelperWriter is run as a child process by TestSeveralProcessesShareOneDatabase.
func TestHelperWriter(t *testing.T) {
	path := os.Getenv("JREP_CACHE_HELPER_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	id, _ := strconv.Atoi(os.Getenv("JREP_CACHE_HELPER_ID"))
	c := open(t, path, "m")
	defer c.Close()
	for i := range 300 {
		k := id*1000 + i
		if i < 100 {
			k = i // shared by every process
		}
		if err := c.Put(key(k), float64(k)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get([]Key{key(k)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeveralProcessesShareOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.db")
	const procs = 6
	cmds := make([]*exec.Cmd, procs)
	for p := range procs {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWriter$", "-test.count=1")
		cmd.Env = append(os.Environ(), "JREP_CACHE_HELPER_DB="+path, fmt.Sprintf("JREP_CACHE_HELPER_ID=%d", p+1))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[p] = cmd
	}
	for p, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("process %d: %v\n%s", p+1, err, cmd.Stdout)
		}
	}

	c := open(t, path, "m")
	defer c.Close()
	var keys []Key
	for i := range 100 {
		keys = append(keys, key(i))
	}
	for p := 1; p <= procs; p++ {
		for i := 100; i < 300; i++ {
			keys = append(keys, key(p*1000+i))
		}
	}
	got, err := c.Get(keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Fatalf("want %d answers, got %d", len(keys), len(got))
	}
}

func TestChunkLayoutsPersistAndAgeOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.db")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) { now = func() time.Time { return t0.Add(d) } }
	defer func() { now = time.Now }()
	day := 24 * time.Hour

	at(0)
	c := open(t, path, "m")
	if err := c.PutChunks(map[Key][]byte{key(1): []byte("one"), key(2): []byte("two")}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutChunks(map[Key][]byte{key(1): []byte("other")}); err != nil {
		t.Fatal(err)
	}
	c.Close()

	at(20 * day)
	c = open(t, path, "other-model") // layouts don't depend on the model
	got, err := c.GetChunks([]Key{key(1), key(3)})
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[key(1)]) != "one" {
		t.Fatalf("got %q", got)
	}

	at(31 * day)
	c = open(t, path, "m")
	defer c.Close()
	got, _ = c.GetChunks([]Key{key(1), key(2)})
	if len(got) != 1 || got[key(2)] != nil {
		t.Fatalf("after 31 days only the layout used on day 20 should be left, got %q", got)
	}
}
