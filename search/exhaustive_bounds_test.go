//go:build !race

package search

// The full bounds of TestEveryAnswerLandsOnItsOwnChunk.
const (
	exChunksTwoQ = 3 // most chunks in a search with 1 or 2 questions
	exChunksOneQ = 4 // most chunks in a search with 1 question
)
