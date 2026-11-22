package search

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

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
	return LoadContext(context.Background(), files, opts)
}

// LoadContext is Load, but stops reading and splitting files once ctx is
// done and returns its error. Layouts split before then are still saved.
func LoadContext(ctx context.Context, files []walk.File, opts LoadOptions) ([]Chunk, error) {
	srcs := make([][]byte, len(files))
	owned := make([]bool, len(files))
	keys := make([]cache.Key, len(files))
	errs := make([]error, len(files))
	version := layoutVersion(opts.Chunk)
	if len(files) > 0 {
		// Pre-hash the shared layoutVersion prefix once so workers skip
		// redundant SHA-256 compression and sync.Pool contention.
		verHash := sha256.New()
		var lenBuf [8]byte
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(version)))
		verHash.Write(lenBuf[:])
		verHash.Write(version)
		var midState [108]byte
		midSlice, err := verHash.(encoding.BinaryAppender).AppendBinary(midState[:0])
		if err != nil {
			return nil, err
		}
		done := ctx.Done()
		var next atomic.Int64
		var wg sync.WaitGroup
		for range min(runtime.GOMAXPROCS(0), len(files)) {
			wg.Go(func() {
				h := sha256.New()
				unm := h.(encoding.BinaryUnmarshaler)
				buf := make([]byte, 0, 256)
				for {
					select {
					case <-done:
						return
					default:
					}
					i := int(next.Add(1)) - 1
					if i >= len(files) {
						return
					}
					src := files[i].Src
					if src == nil {
						var err error
						src, err = os.ReadFile(files[i].Path)
						if errors.Is(err, fs.ErrNotExist) {
							continue
						}
						if err != nil {
							errs[i] = err
							continue
						}
						owned[i] = true
					}
					srcs[i] = src
					// The name picks the grammar, so it is part of the key.
					base := filepath.Base(files[i].Path)
					_ = unm.UnmarshalBinary(midSlice)
					buf = binary.LittleEndian.AppendUint64(buf[:0], uint64(len(base)))
					buf = append(buf, base...)
					buf = binary.LittleEndian.AppendUint64(buf, uint64(len(src)))
					h.Write(buf)
					h.Write(src)
					buf = h.Sum(buf[:0])
					keys[i] = cache.Key(buf)
				}
			})
		}
		wg.Wait()
		if next.Load() < int64(len(files)) {
			return nil, ctx.Err()
		}
	}
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

	// Biggest files first: parse time grows with size, and one slow file
	// started last would hold up the whole load while the other workers
	// sit idle.
	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(len(srcs[b]), len(srcs[a])) })

	per := make([][]chunk.Chunk, len(files))
	fresh := make([][]byte, len(files)) // layouts to save
	stopped := each(ctx, len(order), func(j int) {
		i := order[j]
		f, src := files[i], srcs[i]
		if src == nil {
			return
		}
		cs, ok := decodeLayoutOwned(layouts[keys[i]], src, owned[i])
		if !ok {
			cs = chunk.Split(f.Path, src, opts.Chunk)
			fresh[i] = encodeLayout(cs)
		}
		per[i] = cs
	})

	if opts.Store != nil {
		save := map[cache.Key][]byte{}
		for i, l := range fresh {
			if l != nil {
				save[keys[i]] = l
			}
		}
		if len(save) > 0 {
			if err := opts.Store.PutChunks(save); err != nil {
				opts.cacheError(fmt.Errorf("saving chunk cache: %w", err))
			}
		}
	}
	if stopped != nil {
		return nil, stopped
	}

	n := 0
	for _, cs := range per {
		n += len(cs)
	}
	out := make([]Chunk, 0, n)
	for i, cs := range per {
		for _, c := range cs {
			out = append(out, Chunk{Path: files[i].Path, Rel: files[i].Rel, Chunk: c})
		}
	}
	return out, nil
}

func (o LoadOptions) cacheError(err error) {
	if o.OnCacheError != nil {
		o.OnCacheError(err)
	}
}

// each runs fn(0) … fn(n-1), in that order, on one worker per CPU. It stops
// starting new calls once ctx is done, and returns ctx's error if any call
// was skipped.
func each(ctx context.Context, n int, fn func(int)) error {
	done := ctx.Done()
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), n) {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		})
	}
	wg.Wait()
	if next.Load() < int64(n) {
		return ctx.Err()
	}
	return nil
}

// layoutMagic identifies the compact binary layout format. Legacy cached
// layouts start with '[' (JSON array), so decodeLayout handles both.
const layoutMagic = 0x01

// layout is one chunk without its code, used for legacy JSON decoding.
type layout struct {
	Start     int    `json:"s"`
	End       int    `json:"e"`
	Offset    int    `json:"o"`
	Len       int    `json:"n"`
	Signature string `json:"g"`
}

// encodeLayout writes cs in compact binary varint format (about 3-4x smaller
// than JSON and ~20x faster to encode/decode, cutting SQLite disk bytes and
// cached load latency).
func encodeLayout(cs []chunk.Chunk) []byte {
	size := 1 + binary.MaxVarintLen64
	for _, c := range cs {
		size += 20 + len(c.Signature)
	}
	b := make([]byte, 0, size)
	b = append(b, layoutMagic)
	b = binary.AppendUvarint(b, uint64(len(cs)))
	prevStart, prevOff := 0, 0
	for _, c := range cs {
		b = binary.AppendVarint(b, int64(c.Start-prevStart))
		b = binary.AppendVarint(b, int64(c.End-c.Start))
		b = binary.AppendVarint(b, int64(c.Offset-prevOff))
		b = binary.AppendUvarint(b, uint64(len(c.Code)))
		b = binary.AppendUvarint(b, uint64(len(c.Signature)))
		b = append(b, c.Signature...)
		prevStart = c.Start
		prevOff = c.Offset
	}
	return b
}

// decodeLayout rebuilds a file's chunks from a stored layout. It fails on a
// missing or damaged layout, and the file is split instead.
func decodeLayout(b []byte, src []byte) ([]chunk.Chunk, bool) {
	return decodeLayoutOwned(b, src, false)
}

func decodeLayoutOwned(b []byte, src []byte, owned bool) ([]chunk.Chunk, bool) {
	if len(b) == 0 {
		return nil, false
	}
	var code string
	if owned {
		code = unsafe.String(unsafe.SliceData(src), len(src))
	} else {
		code = string(src)
	}
	if b[0] == layoutMagic {
		return decodeBinaryLayout(b[1:], code)
	}
	var ls []layout
	if json.Unmarshal(b, &ls) != nil {
		return nil, false
	}
	cs := make([]chunk.Chunk, len(ls))
	for i, l := range ls {
		if l.Offset < 0 || l.Len < 0 || l.Offset > len(code) || l.Len > len(code)-l.Offset {
			return nil, false
		}
		cs[i] = chunk.Chunk{Start: l.Start, End: l.End, Signature: l.Signature, Code: code[l.Offset : l.Offset+l.Len], Offset: l.Offset}
	}
	return cs, true
}

func decodeBinaryLayout(b []byte, code string) ([]chunk.Chunk, bool) {
	count, n := binary.Uvarint(b)
	if n <= 0 || count > uint64(len(b)+1) {
		return nil, false
	}
	// Convert the payload once into a single immutable string so every chunk
	// Signature is a zero-allocation substring instead of allocating per chunk.
	rawStr := string(b)
	pos := n
	cs := make([]chunk.Chunk, count)
	prevStart, prevOff := 0, 0
	for i := range cs {
		ds, n := binary.Varint(b[pos:])
		if n <= 0 {
			return nil, false
		}
		pos += n
		de, n := binary.Varint(b[pos:])
		if n <= 0 {
			return nil, false
		}
		pos += n
		do, n := binary.Varint(b[pos:])
		if n <= 0 {
			return nil, false
		}
		pos += n
		codeLen, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, false
		}
		pos += n
		sigLen, n := binary.Uvarint(b[pos:])
		if n <= 0 || sigLen > uint64(len(b)-pos-n) {
			return nil, false
		}
		pos += n
		sig := rawStr[pos : pos+int(sigLen)]
		pos += int(sigLen)

		start := prevStart + int(ds)
		end := start + int(de)
		off := prevOff + int(do)
		if off < 0 || off > len(code) || codeLen > uint64(len(code)-off) {
			return nil, false
		}
		cs[i] = chunk.Chunk{
			Start:     start,
			End:       end,
			Signature: sig,
			Code:      code[off : off+int(codeLen)],
			Offset:    off,
		}
		prevStart = start
		prevOff = off
	}
	if pos != len(b) {
		return nil, false
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
