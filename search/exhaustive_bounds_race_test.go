//go:build race

package search

// The bounds of TestEveryAnswerLandsOnItsOwnChunk under the race detector,
// one chunk smaller than the full ones: at full size the race run takes
// minutes.
const (
	exChunksTwoQ = 2
	exChunksOneQ = 3
)
