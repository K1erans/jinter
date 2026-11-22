package prefilter

import (
	"strings"
	"testing"
)

// chunkCode is about 2 KB of ordinary code holding none of the keywords, so
// Keep has to scan all of it.
var chunkCode = strings.Repeat(`func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	order, err := decodeOrder(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.queue.Push(order)
}
`, 10)

func BenchmarkKeep(b *testing.B) {
	f, err := New("", []string{"where do we retry failed payments?"})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(chunkCode)))
	for b.Loop() {
		if f.Keep(chunkCode) {
			b.Fatal("chunk should not match")
		}
	}
}
