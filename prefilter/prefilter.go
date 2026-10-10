// Package prefilter is the optional keyword pre-pass: a local,
// case-insensitive match that decides which chunks are worth sending to Jev.
package prefilter

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

type Filter struct{ re *regexp.Regexp }

// New uses pattern as the keyword regex, or builds one from the questions'
// words when pattern is empty.
func New(pattern string, questions []string) (*Filter, error) {
	if pattern == "" {
		words := Keywords(questions)
		if len(words) == 0 {
			return nil, errors.New("no keywords left in the question after dropping common words; pass keywords")
		}
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		pattern = strings.Join(words, "|")
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, err
	}
	return &Filter{re: re}, nil
}

// Keep reports whether any keyword appears in code.
func (f *Filter) Keep(code string) bool { return f.re.MatchString(code) }

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
