package walk

import (
	"path"
	"regexp"
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

// testName matches test files by the naming conventions of common
// languages: Go, Python and C++ _test / _unittest; Python test_*, tests.py
// and conftest; JS/TS .test, .spec and Cypress .cy; Ruby _spec; and
// FooTest(s), FooIT and FooSpec in Java, Kotlin, Scala, C#, PHP and Swift.
var testName = regexp.MustCompile(`(_(unit)?test\.\w+|^test_.*\.py|^tests?\.py|^conftest\.py|\.(test|spec|cy)\.[cm]?[jt]sx?|_spec\.rb|[a-z0-9](Tests?|IT|Spec)\.(java|kt|scala|cs|php|swift))$`)

// IsTest reports whether a slash path looks like a test file or test data.
// Agents searching for behaviour almost always want the implementation;
// tests describe the same behaviour and crowd it out of the results.
func IsTest(rel string) bool {
	dir, name := path.Split(rel)
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		part = strings.ToLower(part)
		if testDirs[part] || strings.HasSuffix(part, ".tests") {
			return true
		}
	}
	return testName.MatchString(name)
}
