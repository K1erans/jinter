package walk

import (
	"path"
	"strings"
)

// IsTest reports whether a slash path looks like a test file or test data.
// Agents searching for behaviour almost always want the implementation;
// tests describe the same behaviour and crowd it out of the results.
func IsTest(rel string) bool {
	dir, name := splitSlash(rel)
	for s := strings.Trim(dir, "/"); s != ""; {
		part := s
		if i := strings.IndexByte(s, '/'); i >= 0 {
			part, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		if isTestDir(part) {
			return true
		}
	}
	return testName(name)
}

func isTestDir(part string) bool {
	if asciiHasSuffixFold(part, ".tests") {
		return true
	}
	switch len(part) {
	case 4:
		return asciiEqualFold(part, "test")
	case 5:
		return asciiEqualFold(part, "tests")
	case 8:
		return asciiEqualFold(part, "testdata") || asciiEqualFold(part, "fixtures")
	case 9:
		return asciiEqualFold(part, "__tests__") || asciiEqualFold(part, "__mocks__")
	case 12:
		return asciiEqualFold(part, "__fixtures__")
	case 13:
		return asciiEqualFold(part, "__snapshots__")
	}
	return false
}

// testName reports whether a file name follows the test naming convention
// of a common language: Go, Python and C++ _test / _unittest; Python
// test_*, tests.py and conftest; JS/TS .test, .spec and Cypress .cy; Ruby
// _spec; and FooTest(s), FooIT and FooSpec in Java, Kotlin, Scala, C#, PHP
// and Swift. Case matters. It is code, not a regexp, because it runs on
// every listed file before any is opened.
func testName(name string) bool {
	ext := path.Ext(name)
	stem := name[:len(name)-len(ext)]
	if (strings.HasSuffix(stem, "_test") || strings.HasSuffix(stem, "_unittest")) && isWord(strings.TrimPrefix(ext, ".")) {
		return true
	}
	switch ext {
	case ".py":
		return strings.HasPrefix(name, "test_") || name == "test.py" || name == "tests.py" || name == "conftest.py"
	case ".rb":
		return strings.HasSuffix(stem, "_spec")
	case ".js", ".jsx", ".ts", ".tsx",
		".cjs", ".cjsx", ".cts", ".ctsx",
		".mjs", ".mjsx", ".mts", ".mtsx":
		switch path.Ext(stem) {
		case ".test", ".spec", ".cy":
			return true
		}
	case ".java", ".kt", ".scala", ".cs", ".php", ".swift":
		for _, suffix := range [...]string{"Test", "Tests", "IT", "Spec"} {
			// The class name before the suffix ends in a lowercase letter
			// or digit, so Test.java and APITest.java don't count.
			if before, ok := strings.CutSuffix(stem, suffix); ok && before != "" && isLowerOrDigit(before[len(before)-1]) {
				return true
			}
		}
	}
	return false
}

// isWord reports whether s is one or more ASCII letters, digits or
// underscores, as an extension must be for a _test file.
func isWord(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !isLowerOrDigit(c) && c != '_' && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func isLowerOrDigit(c byte) bool { return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' }
