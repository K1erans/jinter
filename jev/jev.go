// Package jev talks to TypeSafe's Jev model. Client is the seam: the HTTP
// client calls the real API, jevtest.Fake answers offline.
package jev

import (
	"context"
	"strings"
)

// State is one chunk as Jev is shown it on its own. It has no line
// numbers: they say nothing about what the code does, and as part of the
// cache key they would make an edit near the top of a file re-ask every
// chunk below it.
type State struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// Chunk is one piece of a file and the questions asked about it.
type Chunk struct {
	Code      string
	Questions []string // search questions; the client phrases them, see Instruction
}

// Request asks about one or more chunks of the file at Path in a single
// call. Every question about a chunk shares the call, so the chunk is paid
// for once, and chunks of one file share it too: each call costs ~240
// tokens before its code, 40% of a scan's tokens.
type Request struct {
	Model  string
	Path   string
	Chunks []Chunk
}

type Response struct {
	Model string      // the resolved version, e.g. "jev-1.13.0"
	Nouls [][]float64 // probability of yes: Nouls[c][q] for Chunks[c].Questions[q]
}

type Client interface {
	Ask(ctx context.Context, req Request) (Response, error)
}

// Instruction phrases a search question as a Noul statement about a lone
// chunk, in the state's code field.
func Instruction(question string) string {
	return instruction("code", question)
}

// instruction phrases a question about the chunk in field.
func instruction(field, question string) string {
	return "The `" + field + "` " + NormalizeSpace(question)
}

// NormalizeSpace collapses runs of white space and trims the ends.
func NormalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
