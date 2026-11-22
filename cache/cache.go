// Package cache stores Jev's answers so repeat searches cost nothing.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"strings"
	"sync"
	"unicode/utf8"
)

// Key is a sha256 (see Hash): of model + question + chunk as sent alone for an
// answer, or of the splitter's version + file name + content for a layout.
type Key [32]byte

// Cache maps keys to Noul probabilities, and file contents to how they
// were split into chunks, so a repeat search needn't parse every file
// again. SQLite is the real store; any other store only has to implement
// these calls.
//
// PutAnswers gets every answer from one Jev call at once, so a store can
// save them in one write. The same key can appear more than once in a
// call, always with the same value: two chunks with identical code in one
// file share a key. Like PutChunks, it keeps an answer already stored for
// a key, since the key pins the exact model, question and code.
type Cache interface {
	Get(keys []Key) (map[Key]float64, error)
	PutAnswers(answers []Answer) error
	GetChunks(keys []Key) (map[Key][]byte, error)
	PutChunks(layouts map[Key][]byte) error
}

// Answer is Jev's probability for the chunk and question its key hashes.
type Answer struct {
	Key  Key
	Noul float64
}

// KeyOf hashes exactly what Jev is asked about one chunk, as if it were
// sent alone, even when it shared a call with others. The question is lowercased and
// its white space collapsed first, so trivially different spellings share
// answers. Parts are length-prefixed so they can't run into each other.
//
// It gives the same key as Hash(model, normalize(question), state), but
// a search makes one key per chunk per question, so it builds the
// normalized question in a pooled buffer instead of allocating.
func KeyOf(model, question string, state []byte) Key {
	s := hashers.Get().(*hasher)
	defer hashers.Put(s)
	s.h.Reset()
	b := binary.LittleEndian.AppendUint64(s.buf[:0], uint64(len(model)))
	b = append(b, model...)
	n := len(b) // the question's length prefix, filled in once it is known
	b = appendNormalized(binary.LittleEndian.AppendUint64(b, 0), question)
	binary.LittleEndian.PutUint64(b[n:], uint64(len(b)-n-8))
	s.h.Write(b)
	s.buf = b
	s.part(state)
	return s.sum()
}

// Hash is sha256 over length-prefixed parts, so parts can't run into each
// other.
func Hash(parts ...[]byte) Key {
	s := hashers.Get().(*hasher)
	defer hashers.Put(s)
	s.h.Reset()
	for _, part := range parts {
		s.part(part)
	}
	return s.sum()
}

// hasher is a sha256 and a scratch buffer, pooled so that keying every
// chunk doesn't allocate.
type hasher struct {
	h   hash.Hash
	buf []byte
}

var hashers = sync.Pool{New: func() any { return &hasher{h: sha256.New(), buf: make([]byte, 0, 256)} }}

// part writes p's length, then p.
func (s *hasher) part(p []byte) {
	s.buf = binary.LittleEndian.AppendUint64(s.buf[:0], uint64(len(p)))
	s.h.Write(s.buf)
	s.h.Write(p)
}

func (s *hasher) sum() Key {
	s.buf = s.h.Sum(s.buf[:0])
	return Key(s.buf)
}

func normalize(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// appendNormalized appends normalize(q) to dst. ASCII questions, which
// is nearly all of them, are done in place; anything else goes through
// normalize, whose Unicode case and space rules this doesn't repeat.
func appendNormalized(dst []byte, q string) []byte {
	start := len(dst)
	space := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c >= utf8.RuneSelf:
			return append(dst[:start], normalize(q)...)
		case c == ' ', c == '\t', c == '\n', c == '\v', c == '\f', c == '\r': // unicode.IsSpace's ASCII spaces
			space = true
			continue
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		if space && len(dst) > start {
			dst = append(dst, ' ')
		}
		space = false
		dst = append(dst, c)
	}
	return dst
}

// Memory is an in-process Cache, for tests and runs that keep nothing.
type Memory struct {
	mu     sync.Mutex
	m      map[Key]float64
	chunks map[Key][]byte
}

func NewMemory() *Memory { return &Memory{m: map[Key]float64{}, chunks: map[Key][]byte{}} }

func (c *Memory) Get(keys []Key) (map[Key]float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[Key]float64{}
	for _, k := range keys {
		if v, ok := c.m[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (c *Memory) PutAnswers(answers []Answer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range answers {
		if _, ok := c.m[a.Key]; !ok {
			c.m[a.Key] = a.Noul
		}
	}
	return nil
}

func (c *Memory) GetChunks(keys []Key) (map[Key][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[Key][]byte{}
	for _, k := range keys {
		if v, ok := c.chunks[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (c *Memory) PutChunks(layouts map[Key][]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range layouts {
		if _, ok := c.chunks[k]; !ok {
			c.chunks[k] = v
		}
	}
	return nil
}
