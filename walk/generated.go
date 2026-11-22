package walk

import (
	"path"
	"strings"
)

// IsGenerated reports whether a slash path is generated or installed code:
// a lockfile, source map, minified bundle or SVG, or anything inside a
// folder of installed third-party code (node_modules, vendor,
// bower_components), which is often left out of .gitignore or committed on
// purpose. They cost Jev requests and never answer a question about what
// the project's code does; in one project a lockfile was 70% of the tokens
// a scan would send, and in another a vendored three.js was 29%. Naming
// such a folder searches it anyway.
func IsGenerated(rel string) bool {
	dir, name := splitSlash(rel)
	if strings.Contains(dir, "node_modules") || strings.Contains(dir, "vendor") || strings.Contains(dir, "bower_components") {
		for s := strings.Trim(dir, "/"); s != ""; {
			part := s
			if i := strings.IndexByte(s, '/'); i >= 0 {
				part, s = s[:i], s[i+1:]
			} else {
				s = ""
			}
			if installedDir(part) {
				return true
			}
		}
	}
	return generatedName(name)
}

// installedDir reports whether a folder name holds installed third-party
// code.
func installedDir(name string) bool {
	return name == "node_modules" || name == "vendor" || name == "bower_components"
}

func splitSlash(rel string) (dir, name string) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

func asciiEqualFold(a, lower string) bool {
	if len(a) != len(lower) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func asciiHasSuffixFold(s, lowerSuffix string) bool {
	if len(s) < len(lowerSuffix) {
		return false
	}
	return asciiEqualFold(s[len(s)-len(lowerSuffix):], lowerSuffix)
}

// generatedName reports whether a file name, in any case, is written by
// tools rather than people: lockfiles (yarn.lock, bun.lockb, Cargo.lock,
// package-lock.json, pnpm-lock.yaml, npm-shrinkwrap.json, go.sum, …),
// source maps, minified bundles and SVG drawings. It is code, not a
// regexp, because it runs on every listed file before any is opened.
func generatedName(name string) bool {
	ext := path.Ext(name)
	if len(ext) < 3 {
		return false
	}
	stem := name[:len(name)-len(ext)]
	switch len(ext) {
	case 3: // .js
		if asciiEqualFold(ext, ".js") {
			return asciiEqualFold(path.Ext(stem), ".min")
		}
	case 4: // .svg, .yml, .sum, .map, .mjs, .cjs, .css
		switch {
		case asciiEqualFold(ext, ".svg"):
			return true
		case asciiEqualFold(ext, ".yml"):
			return asciiHasSuffixFold(stem, ".lock") || asciiHasSuffixFold(stem, "-lock")
		case asciiEqualFold(ext, ".sum"):
			return asciiEqualFold(name, "go.sum") || asciiEqualFold(name, "go.work.sum")
		case asciiEqualFold(ext, ".map"):
			switch ext2 := path.Ext(stem); len(ext2) {
			case 3:
				return asciiEqualFold(ext2, ".js") || asciiEqualFold(ext2, ".ts")
			case 4:
				return asciiEqualFold(ext2, ".mjs") || asciiEqualFold(ext2, ".cjs") ||
					asciiEqualFold(ext2, ".jsx") || asciiEqualFold(ext2, ".mts") ||
					asciiEqualFold(ext2, ".cts") || asciiEqualFold(ext2, ".tsx") ||
					asciiEqualFold(ext2, ".css")
			}
		case asciiEqualFold(ext, ".mjs"), asciiEqualFold(ext, ".cjs"), asciiEqualFold(ext, ".css"):
			return asciiEqualFold(path.Ext(stem), ".min")
		}
	case 5: // .lock, .json, .yaml
		switch {
		case asciiEqualFold(ext, ".lock"):
			return true
		case asciiEqualFold(ext, ".json"):
			return asciiHasSuffixFold(stem, ".lock") || asciiHasSuffixFold(stem, "-lock") || asciiEqualFold(name, "npm-shrinkwrap.json")
		case asciiEqualFold(ext, ".yaml"):
			return asciiHasSuffixFold(stem, ".lock") || asciiHasSuffixFold(stem, "-lock")
		}
	case 6: // .lockb
		return asciiEqualFold(ext, ".lockb")
	case 9: // .lockfile
		return asciiEqualFold(ext, ".lockfile")
	}
	return false
}
