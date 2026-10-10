package walk

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// rule is one compiled .gitignore line.
type rule struct {
	base    string // folder holding the .gitignore
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
	anchor  bool // matched against the path from base, not just the name
}

// ruleSet is the ordered list of rules in effect in one folder: parent rules
// first, so later (deeper, later-written) rules win, as in git.
type ruleSet struct{ rules []rule }

func (rs *ruleSet) ignored(path string, isDir bool) bool {
	if rs == nil {
		return false
	}
	ignored := false
	for _, r := range rs.rules {
		if r.dirOnly && !isDir {
			continue
		}
		subject := filepath.Base(path)
		if r.anchor {
			rel, err := filepath.Rel(r.base, path)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			subject = filepath.ToSlash(rel)
		}
		if r.re.MatchString(subject) {
			ignored = !r.negate
		}
	}
	return ignored
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
	f, err := os.Open(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []rule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if r, ok := parseRule(dir, sc.Text()); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

func parseRule(base, line string) (rule, bool) {
	line = strings.TrimRight(line, " \t\r")
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
	re, err := regexp.Compile("^" + globToRegexp(line) + "$")
	if err != nil {
		return rule{}, false
	}
	r.re = re
	return r, true
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
