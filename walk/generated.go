package walk

import (
	"path"
	"strings"
)

// IsGenerated reports whether a slash path is generated or installed code:
// a lockfile, source map, minified bundle or SVG, or anything inside
// node_modules, which is often left out of .gitignore. They cost Jev
// requests and never answer a question about what the code does; in one
// project a lockfile was 70% of the tokens a scan would send.
func IsGenerated(rel string) bool {
	dir, name := path.Split(rel)
	for part := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if part == "node_modules" {
			return true
		}
	}
	return generatedName(name)
}

// generatedName reports whether a file name, in any case, is written by
// tools rather than people: lockfiles (yarn.lock, bun.lockb, Cargo.lock,
// package-lock.json, pnpm-lock.yaml, npm-shrinkwrap.json, go.sum, …),
// source maps, minified bundles and SVG drawings. It is code, not a
// regexp, because it runs on every listed file before any is opened.
func generatedName(name string) bool {
	name = strings.ToLower(name)
	ext := path.Ext(name)
	stem := name[:len(name)-len(ext)]
	switch ext {
	case ".lock", ".lockb", ".lockfile", ".svg":
		return true
	case ".json", ".yaml", ".yml":
		return strings.HasSuffix(stem, ".lock") || strings.HasSuffix(stem, "-lock") || name == "npm-shrinkwrap.json"
	case ".sum":
		return name == "go.sum" || name == "go.work.sum"
	case ".map":
		switch path.Ext(stem) {
		case ".js", ".mjs", ".cjs", ".jsx", ".ts", ".mts", ".cts", ".tsx", ".css":
			return true
		}
	case ".js", ".mjs", ".cjs", ".css":
		return path.Ext(stem) == ".min"
	}
	return false
}
