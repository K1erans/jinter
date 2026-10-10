package walk

import (
	"path"
	"regexp"
	"strings"
)

// generatedName matches files written by tools rather than people:
// lockfiles (yarn.lock, Cargo.lock, package-lock.json, pnpm-lock.yaml,
// go.sum, …), source maps, minified bundles and SVG drawings.
var generatedName = regexp.MustCompile(`(?i)(\.lockb?|[.-]lock\.(json|ya?ml)|\.lockfile|^npm-shrinkwrap\.json|^go\.(sum|work\.sum)|\.(js|mjs|cjs|jsx|ts|mts|cts|tsx|css)\.map|\.min\.(js|mjs|cjs|css)|\.svg)$`)

// IsGenerated reports whether a slash path is generated or installed code:
// a lockfile, source map, minified bundle or SVG, or anything inside
// node_modules, which is often left out of .gitignore. They cost Jev
// requests and never answer a question about what the code does; in one
// project a lockfile was 70% of the tokens a scan would send.
func IsGenerated(rel string) bool {
	dir, name := path.Split(rel)
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "node_modules" {
			return true
		}
	}
	return generatedName.MatchString(name)
}
