package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/chunk"
	"github.com/K1erans/jinter/walk"
)

type LoadOptions struct {
	Chunk chunk.Options

	// Store keeps how each file's content was split, so a repeat search
	// doesn't parse unchanged files again; parsing is most of the time a
	// fully cached search takes. Nil means no cache.
	Store     cache.Cache
	ReadCache bool // false still saves layouts, but never reads them

	// OnCacheError hears about failed cache reads and writes, which don't
	// stop a load.
	OnCacheError func(error)
}

// Load reads (unless the walk already did) and chunks files in parallel,
// keeping file order. A file that vanished since it was listed is skipped.
func Load(files []walk.File, opts LoadOptions) ([]Chunk, error) {
	srcs := make([][]byte, len(files))
	keys := make([]cache.Key, len(files))
	errs := make([]error, len(files))
	version := layoutVersion(opts.Chunk)
	each(len(files), func(i int) {
		src := files[i].Src
		if src == nil {
			var err error
			src, err = os.ReadFile(files[i].Path)
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			if err != nil {
				errs[i] = err
				return
			}
		}
		srcs[i] = src
		// The name picks the grammar, so it is part of the key.
		keys[i] = cache.Hash(version, []byte(filepath.Base(files[i].Path)), src)
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	var layouts map[cache.Key][]byte
	if opts.Store != nil && opts.ReadCache {
		var err error
		if layouts, err = opts.Store.GetChunks(keys); err != nil {
			opts.cacheError(fmt.Errorf("reading chunk cache: %w", err))
		}
	}

	per := make([][]Chunk, len(files))
	fresh := make([][]byte, len(files)) // layouts to save
	each(len(files), func(i int) {
		f, src := files[i], srcs[i]
		if src == nil {
			return
		}
		cs, ok := decodeLayout(layouts[keys[i]], src)
		if !ok {
			cs = chunk.Split(f.Path, src, opts.Chunk)
			fresh[i] = encodeLayout(cs)
		}
		per[i] = make([]Chunk, len(cs))
		for j, c := range cs {
			per[i][j] = Chunk{Path: f.Path, Rel: f.Rel, Chunk: c}
		}
	})

	if opts.Store != nil {
		save := map[cache.Key][]byte{}
		for i, l := range fresh {
			if l != nil {
				save[keys[i]] = l
			}
		}
		if err := opts.Store.PutChunks(save); err != nil {
			opts.cacheError(fmt.Errorf("saving chunk cache: %w", err))
		}
	}

	var out []Chunk
	for _, cs := range per {
		out = append(out, cs...)
	}
	return out, nil
}

func (o LoadOptions) cacheError(err error) {
	if o.OnCacheError != nil {
		o.OnCacheError(err)
	}
}

// each runs fn(0) … fn(n-1) on one worker per CPU.
func each(n int, fn func(int)) {
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
}

// layout is one chunk without its code, which is read back from the
// file at Offset.
type layout struct {
	Start     int    `json:"s"`
	End       int    `json:"e"`
	Offset    int    `json:"o"`
	Len       int    `json:"n"`
	Signature string `json:"g"`
}

func encodeLayout(cs []chunk.Chunk) []byte {
	ls := make([]layout, len(cs))
	for i, c := range cs {
		ls[i] = layout{Start: c.Start, End: c.End, Offset: c.Offset, Len: len(c.Code), Signature: c.Signature}
	}
	b, _ := json.Marshal(ls) // can't fail: plain ints and strings
	return b
}

// decodeLayout rebuilds a file's chunks from a stored layout. It fails on a
// missing or damaged layout, and the file is split instead.
func decodeLayout(b []byte, src []byte) ([]chunk.Chunk, bool) {
	var ls []layout
	if b == nil || json.Unmarshal(b, &ls) != nil {
		return nil, false
	}
	code := string(src)
	cs := make([]chunk.Chunk, len(ls))
	for i, l := range ls {
		if l.Offset < 0 || l.Len < 0 || l.Offset+l.Len > len(code) {
			return nil, false
		}
		cs[i] = chunk.Chunk{Start: l.Start, End: l.End, Signature: l.Signature, Code: code[l.Offset : l.Offset+l.Len], Offset: l.Offset}
	}
	return cs, true
}

// layoutVersion is hashed into every layout key: the splitter's version,
// its options and the tree-sitter release, any of which can change how the
// same file splits.
func layoutVersion(opts chunk.Options) []byte {
	return fmt.Appendf(nil, "chunks %d %+v %s", chunk.Version, opts, treeSitterVersion())
}

var treeSitterVersion = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, m := range info.Deps {
			if m.Path == "github.com/odvcencio/gotreesitter" {
				if m.Replace != nil {
					m = m.Replace
				}
				return m.Version + " " + m.Sum
			}
		}
	}
	return ""
})
