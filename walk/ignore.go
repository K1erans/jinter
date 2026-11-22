package walk

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type ruleKind uint8

const (
	ruleRegex  ruleKind = iota
	ruleExact           // exact match: subject == pat
	ruleSuffix          // "*.ext" (no slash): strings.HasSuffix(subject, pat) && !strings.Contains(subject, "/")
	rulePrefix          // "foo/**" anchored: subject == pat || strings.HasPrefix(subject, pat+"/")
)

// rule is one compiled .gitignore line.
type rule struct {
	base    string // folder holding the .gitignore
	kind    ruleKind
	pat     string
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	anchor  bool // matched against the path from base, not just the name
}

func (r *rule) match(subject string) bool {
	switch r.kind {
	case ruleExact:
		return subject == r.pat
	case ruleSuffix:
		return strings.HasSuffix(subject, r.pat)
	case rulePrefix:
		return len(subject) > len(r.pat) && subject[len(r.pat)] == '/' && strings.HasPrefix(subject, r.pat)
	default:
		return r.re.MatchString(subject)
	}
}

// ruleSet is the ordered list of rules in effect in one folder: parent rules
// first, so later (deeper, later-written) rules win, as in git.
type ruleSet struct{ rules []rule }

func (rs *ruleSet) ignored(path string, isDir bool) bool {
	if rs == nil {
		return false
	}
	name := baseName(path)
	// The last matching rule decides, so search from the end. Rules from
	// one .gitignore share a base and sit together, so path's rel from a
	// base is worked out once, not once per anchored rule.
	var base, rel string
	for i := len(rs.rules) - 1; i >= 0; i-- {
		r := &rs.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		subject := name
		if r.anchor {
			if r.base != base {
				base, rel = r.base, relUnder(r.base, path)
			}
			if rel == "" {
				continue // path isn't under the rule's folder
			}
			subject = rel
		}
		if r.match(subject) {
			return !r.negate
		}
	}
	return false
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, filepath.Separator); i >= 0 {
		return p[i+1:]
	}
	return p
}

// relUnder returns path's slash path relative to base, or "" if path is not
// under base. Avoids filepath.Rel on the common clean prefix case.
func relUnder(base, path string) string {
	if base == "." {
		if path == "." || escapes(path) {
			return ""
		}
		if filepath.Separator == '/' {
			return path
		}
		return filepath.ToSlash(path)
	}
	if len(path) > len(base) && path[len(base)] == filepath.Separator && path[:len(base)] == base {
		sub := path[len(base)+1:]
		if filepath.Separator == '/' {
			return sub
		}
		return filepath.ToSlash(sub)
	}
	if p, err := filepath.Rel(base, path); err == nil && !escapes(p) {
		return filepath.ToSlash(p)
	}
	return ""
}

// with returns the rules for dir: rs plus dir's own .gitignore, if any.
func (rs *ruleSet) with(dir string) *ruleSet {
	own := readGitignore(dir)
	if len(own) == 0 {
		return rs
	}
	out := &ruleSet{}
	if rs != nil {
		out.rules = append(out.rules, rs.rules...)
	}
	out.rules = append(out.rules, own...)
	return out
}

func readGitignore(dir string) []rule {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	var rules []rule
	for line := range strings.Lines(string(data)) {
		if r, ok := parseRule(dir, line); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

func parseRule(base, line string) (rule, bool) {
	line = strings.TrimRight(line, " \t\r\n")
	if line == "" || strings.HasPrefix(line, "#") {
		return rule{}, false
	}
	r := rule{base: base}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\`) {
		line = line[1:] // \# and \! escape a leading # or !
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	// A slash anywhere but the end ties the pattern to the .gitignore's folder.
	r.anchor = strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return rule{}, false
	}
	// Fast non-regex matchers for the common .gitignore shapes.
	if !hasGlobMeta(line) {
		r.kind = ruleExact
		r.pat = line
		return r, true
	}
	if !r.anchor && strings.HasPrefix(line, "*") && !hasGlobMeta(line[1:]) {
		r.kind = ruleSuffix
		r.pat = line[1:]
		return r, true
	}
	if r.anchor && strings.HasSuffix(line, "/**") && !hasGlobMeta(line[:len(line)-3]) {
		r.kind = rulePrefix
		r.pat = line[:len(line)-3]
		return r, true
	}
	re, err := regexp.Compile("^" + globToRegexp(line) + "$")
	if err != nil {
		return rule{}, false
	}
	r.kind = ruleRegex
	r.re = re
	return r, true
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, `*?[\`)
}

func globToRegexp(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case strings.HasPrefix(glob[i:], "**/") && (i == 0 || glob[i-1] == '/'):
			b.WriteString("(?:.*/)?")
			i += 2
		case glob[i:] == "/**":
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(glob):
			i++
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		case c == '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
