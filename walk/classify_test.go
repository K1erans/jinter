package walk

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The name checks were regexps once; they are code now because they run
// on every listed file and the regexps were most of a walk's CPU time.
// These are the regexps they replaced, kept as the specification.
var (
	generatedNameSpec = regexp.MustCompile(`(?i)(\.lockb?|[.-]lock\.(json|ya?ml)|\.lockfile|^npm-shrinkwrap\.json|^go\.(sum|work\.sum)|\.(js|mjs|cjs|jsx|ts|mts|cts|tsx|css)\.map|\.min\.(js|mjs|cjs|css)|\.svg)$`)
	testNameSpec      = regexp.MustCompile(`(_(unit)?test\.\w+|^test_.*\.py|^tests?\.py|^conftest\.py|\.(test|spec|cy)\.[cm]?[jt]sx?|_spec\.rb|[a-z0-9](Tests?|IT|Spec)\.(java|kt|scala|cs|php|swift))$`)
)

// specNames builds file names from pieces of the conventions the checks
// know, so near misses as well as matches are compared.
func specNames() []string {
	starts := []string{"", "a", "A", "foo", "Foo", "x.", ".", "-", "_", "1", "test_", "Test", "go", "go.work", "npm-shrinkwrap", "conftest", "test", "tests"}
	middles := []string{
		"", "_test", "_unittest", "_Test", "test", "Test", "Tests", "IT", "Spec", "spec", "_spec",
		".test", ".spec", ".cy", ".Test", "-lock", ".lock", "lock", ".LOCK", ".min", ".MIN",
		".js", ".ts", ".css", ".tsx", ".json", ".sum", ".work", "_test.d",
	}
	ends := []string{
		"", ".", ".go", ".py", ".rb", ".c", ".cc", ".x_y", ".x-y", ".go~",
		".js", ".jsx", ".ts", ".tsx", ".cjs", ".mjs", ".cts", ".mts", ".mtsx", ".cjsx", ".tjs", ".mmjs",
		".java", ".kt", ".scala", ".cs", ".php", ".swift", ".Java",
		".json", ".JSON", ".yaml", ".yml", ".YML", ".lock", ".lockb", ".lockfile", ".Lock",
		".sum", ".map", ".Map", ".css", ".svg", ".SVG", ".svgz",
	}
	var out []string
	for _, s := range starts {
		for _, m := range middles {
			for _, e := range ends {
				out = append(out, s+m+e)
			}
		}
	}
	return out
}

func TestNameChecksMatchTheirSpec(t *testing.T) {
	names := specNames()
	matched := map[string]int{}
	for _, name := range names {
		if got, want := generatedName(name), generatedNameSpec.MatchString(name); got != want {
			t.Errorf("generatedName(%q) = %v, spec says %v", name, got, want)
		} else if got {
			matched["generated"]++
		}
		if got, want := testName(name), testNameSpec.MatchString(name); got != want {
			t.Errorf("testName(%q) = %v, spec says %v", name, got, want)
		} else if got {
			matched["test"]++
		}
	}
	// Guard the corpus itself: it must hold plenty of each answer.
	for _, kind := range []string{"generated", "test"} {
		if n := matched[kind]; n < 200 || n > len(names)-200 {
			t.Errorf("%d of %d names are %s; the corpus doesn't exercise both answers", n, len(names), kind)
		}
	}
}

func FuzzNameChecksMatchTheirSpec(f *testing.F) {
	for _, name := range []string{"yarn.lock", "package-lock.json", "app.js.map", "x.min.css", "feed_test.go", "test_api.py", "app.spec.tsx", "FooTests.swift", "APITest.java"} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if strings.Contains(name, "/") || !utf8.ValidString(name) {
			return
		}
		if got, want := testName(name), testNameSpec.MatchString(name); got != want {
			t.Errorf("testName(%q) = %v, spec says %v", name, got, want)
		}
		// (?i) also folds non-ASCII lookalikes such as ſ (long s) into s,
		// which strings.ToLower doesn't; no real lockfile is named so.
		for _, r := range name {
			if r >= utf8.RuneSelf {
				return
			}
		}
		if got, want := generatedName(name), generatedNameSpec.MatchString(name); got != want {
			t.Errorf("generatedName(%q) = %v, spec says %v", name, got, want)
		}
	})
}
