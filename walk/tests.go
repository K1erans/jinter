package walk

import (
	"path"
	"strings"
)

// testDirs hold tests, mocks, fixtures or test data in common layouts.
// Matched case-insensitively, so Swift's Tests/ counts too. RSpec's spec/
// is left out because API specs live in folders of that name; RSpec files
// are caught by their _spec.rb names instead.
var testDirs = map[string]bool{
	"test": true, "tests": true, "__tests__": true, "__mocks__": true,
	"testdata": true, "fixtures": true, "__fixtures__": true, "__snapshots__": true,
}

// IsTest reports whether a slash path looks like a test file or test data.
// Agents searching for behaviour almost always want the implementation;
// tests describe the same behaviour and crowd it out of the results.
func IsTest(rel string) bool {
	dir, name := path.Split(rel)
	for part := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		part = strings.ToLower(part)
		if testDirs[part] || strings.HasSuffix(part, ".tests") {
			return true
		}
	}
	return testName(name)
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
		for _, suffix := range []string{"Test", "Tests", "IT", "Spec"} {
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
