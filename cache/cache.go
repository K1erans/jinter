// Package cache stores Jev's answers so repeat searches cost nothing.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
)

// Key is a sha256 (see Hash): of model + question + chunk as sent alone for an
// answer, or of the splitter's version + file name + content for a layout.
type Key [32]byte

// Cache maps keys to Noul probabilities, and file contents to how they
// were split into chunks, so a repeat search needn't parse every file
// again. SQLite is the real store; any other store only has to implement
// these calls.
type Cache interface {
	Get(keys []Key) (map[Key]float64, error)
	Put(key Key, noul float64) error
	GetChunks(keys []Key) (map[Key][]byte, error)
	PutChunks(layouts map[Key][]byte) error
}

// KeyOf hashes exactly what Jev is asked about one chunk, as if it were
// sent alone, even when it shared a call with others. The question is lowercased and
// its white space collapsed first, so trivially different spellings share
// answers. Parts are length-prefixed so they can't run into each other.
func KeyOf(model, question string, state []byte) Key {
	return Hash([]byte(model), []byte(normalize(question)), state)
}

// Hash is sha256 over length-prefixed parts, so parts can't run into each
// other.
func Hash(parts ...[]byte) Key {
	h := sha256.New()
	for _, part := range parts {
		h.Write(binary.LittleEndian.AppendUint64(nil, uint64(len(part))))
		h.Write(part)
	}
	var k Key
	h.Sum(k[:0])
	return k
}

func normalize(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
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

func (c *Memory) Put(key Key, noul float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		c.m[key] = noul
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
