// Package jevtest provides a deterministic offline Jev client for tests.
package jevtest

import (
	"context"
	"strings"
	"sync"

	"github.com/K1erans/jinter/jev"
)

// Fake answers each question about each chunk with Score(state, question),
// where state is the chunk as it would be sent alone.
type Fake struct {
	Score func(state jev.State, question string) float64
	Err   error // returned by every call when set

	mu       sync.Mutex
	requests []jev.Request
}

func (f *Fake) Ask(ctx context.Context, req jev.Request) (jev.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return jev.Response{}, err
	}
	if f.Err != nil {
		return jev.Response{}, f.Err
	}
	resp := jev.Response{Model: req.Model}
	for _, c := range req.Chunks {
		nouls := make([]float64, len(c.Questions))
		for i, q := range c.Questions {
			nouls[i] = f.Score(jev.State{Path: req.Path, Code: c.Code}, q)
		}
		resp.Nouls = append(resp.Nouls, nouls)
	}
	return resp, nil
}

// Requests returns every request received so far.
func (f *Fake) Requests() []jev.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]jev.Request(nil), f.requests...)
}

// Contains scores 0.9 when the code contains word (case-insensitive) for
// the question, and 0.1 otherwise.
func Contains(words map[string]string) func(jev.State, string) float64 {
	return func(s jev.State, q string) float64 {
		w, ok := words[q]
		if ok && strings.Contains(strings.ToLower(s.Code), strings.ToLower(w)) {
			return 0.9
		}
		return 0.1
	}
}
