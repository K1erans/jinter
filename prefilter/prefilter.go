// Package prefilter is the optional keyword pre-pass: a local,
// case-insensitive match that decides which chunks are worth sending to Jev.
package prefilter

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type Filter struct {
	re *regexp.Regexp
	// words are the keywords, case-folded by fold, when the regex is built
	// from them. Keep searches for them directly: it matches exactly what
	// the regex would, many times faster.
	words [][]byte
}

// New uses pattern as the keyword regex, or builds one from the questions'
// words when pattern is empty.
func New(pattern string, questions []string) (*Filter, error) {
	var words [][]byte
	if pattern == "" {
		kws := Keywords(questions)
		if len(kws) == 0 {
			return nil, errors.New("no keywords left in the question after dropping common words; pass keywords")
		}
		quoted := make([]string, len(kws))
		for i, w := range kws {
			quoted[i] = regexp.QuoteMeta(w)
			words = append(words, fold(nil, w))
		}
		pattern = strings.Join(quoted, "|")
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, err
	}
	return &Filter{re: re, words: words}, nil
}

// Keep reports whether any keyword appears in code.
func (f *Filter) Keep(code string) bool {
	if f.words == nil {
		return f.re.MatchString(code)
	}
	buf := bufs.Get().(*[]byte)
	defer bufs.Put(buf)
	*buf = fold((*buf)[:0], code)
	for _, w := range f.words {
		if bytes.Contains(*buf, w) {
			return true
		}
	}
	return false
}

var bufs = sync.Pool{New: func() any { return new([]byte) }}

// fold appends s to dst with every rune replaced by the smallest rune it
// case-folds to, and invalid UTF-8 by U+FFFD, as the regex reads it. Two
// runes match under (?i) exactly when they fold to the same rune, so a
// keyword matches under (?i) exactly when its folded bytes appear in the
// folded code.
func fold(dst []byte, s string) []byte {
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			// Copy the run of ASCII, then upper-case it in place.
			j := i + 1
			for j < len(s) && s[j] < utf8.RuneSelf {
				j++
			}
			n := len(dst)
			dst = append(dst, s[i:j]...)
			for k, c := range dst[n:] {
				if 'a' <= c && c <= 'z' {
					dst[n+k] = c - ('a' - 'A')
				}
			}
			i = j
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		dst = utf8.AppendRune(dst, least)
	}
	return dst
}

// Pattern is the regex in use, for messages.
func (f *Filter) Pattern() string { return strings.TrimPrefix(f.re.String(), "(?i)") }

// Keywords are the questions' words, lowercased, without stopwords, cut to
// a crude stem so "payments" also finds "payment" and "failed" finds "fail".
func Keywords(questions []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range questions {
		for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
		}) {
			if len(w) < 2 || stopwords[w] {
				continue
			}
			w = stem(w)
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	return out
}

// stem trims words by up to three letters, never below four:
// "failed" → "fail", "sends" → "send", "payments" → "payme".
func stem(w string) string {
	r := []rune(w)
	if len(r) <= 4 {
		return w
	}
	return string(r[:max(4, len(r)-3)])
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		a about all also an and any are as at be been being but by can code
		could did do does doing done each else every file files find for from
		function functions get gets has have how if in into is it its logic
		me method methods my no not of on one or our out part place should so
		some that the their them then there these this those to up us use
		used uses using was we were what when where whether which while who
		why will with would you your`) {
		m[w] = true
	}
	return m
}()
