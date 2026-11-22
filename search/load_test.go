package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

func TestChunksKeepFileOrderWhateverTheirSize(t *testing.T) {
	fs := files(t)
	fs[0], fs[1] = fs[1], fs[0] // the small file first; the big one is split first
	got, err := Load(fs, LoadOptions{Chunk: small()})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range got {
		paths = append(paths, filepath.Base(c.Path))
	}
	if want := []string{"send.go", "retry.go", "retry.go"}; !slices.Equal(paths, want) {
		t.Fatalf("got %q, want %q", paths, want)
	}
}

func TestACancelledLoadStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadContext(ctx, files(t), LoadOptions{Chunk: small()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestDamagedLayoutsAreSplitAgain(t *testing.T) {
	src := []byte("package p\n")
	for _, b := range []string{
		`not json`,
		`[{"o":-1,"n":1}]`,
		`[{"o":5,"n":20}]`,
		`[{"o":9223372036854775807,"n":1}]`, // offset + length overflows
	} {
		if _, ok := decodeLayout([]byte(b), src); ok {
			t.Errorf("%s: accepted", b)
		}
	}
	if cs, ok := decodeLayout([]byte(`[{"s":1,"e":1,"o":0,"n":9,"g":"p"}]`), src); !ok || cs[0].Code != "package p" {
		t.Fatalf("a good layout: %+v, %v", cs, ok)
	}
}
