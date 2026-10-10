package search

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/walk"
)

// layoutCounter is a store that counts the chunk layouts written to it.
type layoutCounter struct {
	*cache.Memory
	written int
}

func (s *layoutCounter) PutChunks(layouts map[cache.Key][]byte) error {
	s.written += len(layouts)
	return s.Memory.PutChunks(layouts)
}

// brokenStore fails every chunk call.
type brokenStore struct{ *cache.Memory }

func (brokenStore) GetChunks([]cache.Key) (map[cache.Key][]byte, error) {
	return nil, errors.New("disk on fire")
}
func (brokenStore) PutChunks(map[cache.Key][]byte) error { return errors.New("disk on fire") }

const retryGo = "package billing\n\n// Retry charges again.\nfunc Retry(id string) error {\n\treturn charge(id)\n}\n\nfunc Refund(id string) error {\n\treturn credit(id)\n}\n"
const sendGo = "package mail\n\nfunc Send(to string) error {\n\treturn smtp(to)\n}\n"

func files(t *testing.T) []walk.File {
	t.Helper()
	dir := t.TempDir()
	var out []walk.File
	for _, f := range []struct{ name, src string }{{"retry.go", retryGo}, {"send.go", sendGo}} {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte(f.src), 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, walk.File{Path: p, Rel: f.name})
	}
	return out
}

func small() chunk.Options {
	o := chunk.Default
	o.MinLines = 1
	return o
}

func TestRepeatLoadsReuseChunkLayouts(t *testing.T) {
	fs := files(t)
	plain, err := Load(fs, LoadOptions{Chunk: small()})
	if err != nil {
		t.Fatal(err)
	}
	store := &layoutCounter{Memory: cache.NewMemory()}
	o := LoadOptions{Chunk: small(), Store: store, ReadCache: true}
	first, err := Load(fs, o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, plain) || store.written != 2 {
		t.Fatalf("first load should split as usual and save 2 layouts, saved %d", store.written)
	}

	again, err := Load(fs, o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, plain) || store.written != 2 {
		t.Fatalf("a repeat load should rebuild the same chunks from the cache, saved %d more", store.written-2)
	}

	os.WriteFile(fs[0].Path, []byte("package billing\n\nfunc Retry(id string) error {\n\treturn chargeTwice(id)\n}\n"), 0o644)
	got, _ := Load(fs, o)
	if store.written != 3 {
		t.Fatalf("only the edited file is split again, saved %d more", store.written-2)
	}
	want, _ := Load(fs, LoadOptions{Chunk: small()})
	if !reflect.DeepEqual(got, want) || !strings.Contains(got[0].Code, "chargeTwice") {
		t.Fatalf("an edited file gets fresh chunks: %+v", got)
	}

	o.ReadCache = false
	if got, _ := Load(fs, o); !reflect.DeepEqual(got, want) {
		t.Fatal("without reading the cache files are split as usual")
	}
}

func TestABrokenChunkCacheOnlyWarns(t *testing.T) {
	fs := files(t)
	var warned []error
	got, err := Load(fs, LoadOptions{Chunk: small(), Store: brokenStore{cache.NewMemory()}, ReadCache: true, OnCacheError: func(err error) { warned = append(warned, err) }})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := Load(fs, LoadOptions{Chunk: small()}); !reflect.DeepEqual(got, want) {
		t.Fatal("files are still split")
	}
	if len(warned) == 0 {
		t.Fatal("the failure should be reported")
	}
}

func TestLayoutsFromATimedOutParseAreSaved(t *testing.T) {
	fs := files(t)
	store := &layoutCounter{Memory: cache.NewMemory()}
	o := small()
	o.ParseTimeout = time.Nanosecond // a runaway parse
	lo := LoadOptions{Chunk: o, Store: store, ReadCache: true}
	got, err := Load(fs, lo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("files still get chunks from the plain splitter")
	}
	if store.written != 2 {
		t.Fatalf("the plain split should be cached so the parse is paid once, saved %d", store.written)
	}
	again, _ := Load(fs, lo)
	if !reflect.DeepEqual(again, got) || store.written != 2 {
		t.Fatalf("a repeat load should reuse the saved split, saved %d more", store.written-2)
	}
}
