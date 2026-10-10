package walk

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tree writes files under dir. Keys are slash paths, values are contents.
func tree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func paths(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func rels(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Rel)
	}
	return out
}

func TestGitRepoFollowsGitignore(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		".gitignore":         "build/\n*.log\n",
		"src/app.go":         "package app\n",
		"src/untracked.go":   "package app\n",
		"build/x.js":         "x\n",
		"debug.log":          "log\n",
		"src/sub/.gitignore": "secret.txt\n",
		"src/sub/secret.txt": "s\n",
		"src/sub/ok.txt":     "ok\n",
	})
	git(t, dir, "init", "-q")
	git(t, dir, "add", ".gitignore", "src/app.go")
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "src/app.go", "src/sub/.gitignore", "src/sub/ok.txt", "src/untracked.go"}
	if got := paths(res.Files); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if resolved, _ := filepath.EvalSymlinks(dir); res.Root != resolved {
		t.Fatalf("root %q, want %q", res.Root, resolved)
	}

	res, err = Walk([]string{"."}, Options{NoIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(res.Files)
	for _, p := range []string{"build/x.js", "debug.log", "src/sub/secret.txt"} {
		if !contains(got, p) {
			t.Errorf("--no-ignore should include %s, got %v", p, got)
		}
	}
	for _, p := range got {
		if strings.HasPrefix(p, ".git/") {
			t.Errorf("never list .git internals, got %s", p)
		}
	}
}

func TestSubdirectoryPathsAreShownAsGivenAndKeyedFromRepoRoot(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{"src/billing/retry.ts": "retry\n", "README.md": "hi\n"})
	git(t, dir, "init", "-q")
	t.Chdir(filepath.Join(dir, "src"))

	res, err := Walk([]string{"billing"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"billing/retry.ts"}) {
		t.Fatalf("display paths %v", got)
	}
	if got := rels(res.Files); !reflect.DeepEqual(got, []string{"src/billing/retry.ts"}) {
		t.Fatalf("repo paths %v", got)
	}
}

func TestOutsideGitParsesGitignoreFiles(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		".gitignore": strings.Join([]string{
			"# build output",
			"/dist",
			"*.out.js",
			"!keep.out.js",
			"logs/",
			"docs/**/draft.md",
		}, "\n"),
		"dist/app.js":         "x",
		"lib/dist/real.js":    "x", // /dist is anchored to the root
		"lib/a.out.js":        "x",
		"lib/keep.out.js":     "x",
		"lib/logs/today.txt":  "x",
		"lib/logs.txt":        "x", // logs/ only matches directories
		"docs/a/b/draft.md":   "x",
		"docs/draft.md":       "x", // **/ also matches zero directories
		"docs/final.md":       "x",
		"pkg/.gitignore":      "*.gen.go\n",
		"pkg/model.gen.go":    "x",
		"pkg/model.go":        "x",
		"other/model.gen.go":  "x", // nested rules apply only below their folder
		".git-not-a-repo.txt": "x",
		"deps/dep.js":         "x",
		"deps/.hidden":        "x",
	})
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{Docs: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".git-not-a-repo.txt", ".gitignore", "deps/.hidden", "deps/dep.js", "docs/final.md",
		"lib/dist/real.js", "lib/keep.out.js", "lib/logs.txt", "other/model.gen.go",
		"pkg/.gitignore", "pkg/model.go",
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if resolved, _ := filepath.EvalSymlinks(dir); res.Root != resolved {
		t.Fatalf("root %q, want the search path %q", res.Root, resolved)
	}
}

func TestSkipsBinariesSilentlyAndReportsOversizeFiles(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		"image.png":   "\x89PNG\x00\x00binary",
		"big.js":      strings.Repeat("a", 2000),
		"big.bin":     strings.Repeat("\x00", 2000),
		"small.go":    "package small\n",
		"exactly.txt": strings.Repeat("b", 1000),
	})
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{MaxFileSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"exactly.txt", "small.go"}) {
		t.Fatalf("files %v", got)
	}
	if got := paths(res.Oversize); !reflect.DeepEqual(got, []string{"big.js"}) {
		t.Fatalf("oversize %v", got)
	}

	res, err = Walk([]string{"."}, Options{MaxFileSize: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"big.js", "exactly.txt", "small.go"}) {
		t.Fatalf("0 means no limit, got %v", got)
	}
}

func TestExplicitFileAndMissingPath(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{".gitignore": "gen.go\n", "gen.go": "package gen\n", "a.go": "package a\n"})
	t.Chdir(dir)

	res, err := Walk([]string{"gen.go", "a.go", "gen.go"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"a.go", "gen.go"}) {
		t.Fatalf("named files are searched once each, even if ignored: %v", got)
	}

	if _, err := Walk([]string{"nope"}, Options{}); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestTestFilesAreSkippedUnlessAsked(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		"feed.go":            "package feed\n",
		"feed_test.go":       "package feed\n",
		"testdata/feed.xml":  "<rss/>\n",
		"web/app.ts":         "x\n",
		"web/app.test.ts":    "x\n",
		"web/__tests__/a.js": "x\n",
		"py/test_parse.py":   "x\n",
		"py/conftest.py":     "x\n",
		"latest/notes.txt":   "x\n", // "test" inside a word isn't a test folder
		"contest.go":         "package feed\n",
	})
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"contest.go", "feed.go", "latest/notes.txt", "web/app.ts"}) {
		t.Fatalf("files %v", got)
	}
	if res.Tests != 6 {
		t.Fatalf("want 6 test files counted as skipped, got %d", res.Tests)
	}

	res, _ = Walk([]string{"."}, Options{Tests: true})
	if len(res.Files) != 10 || res.Tests != 0 {
		t.Fatalf("--tests should include all 10 files, got %v (skipped %d)", paths(res.Files), res.Tests)
	}

	res, _ = Walk([]string{"feed_test.go"}, Options{})
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"feed_test.go"}) {
		t.Fatalf("a test file named explicitly is searched: %v", got)
	}

	res, _ = Walk([]string{"web/__tests__", "testdata"}, Options{})
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"testdata/feed.xml", "web/__tests__/a.js"}) {
		t.Fatalf("a test folder named explicitly is searched: %v", got)
	}
}

func TestOnlyTextTestFilesAreCounted(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n", "testdata/logo.png": "\x89PNG\x00\x00"})
	t.Chdir(dir)
	res, _ := Walk([]string{"."}, Options{})
	if res.Tests != 1 {
		t.Fatalf("binary test data isn't a skipped test file; got %d", res.Tests)
	}
}

func TestIsTest(t *testing.T) {
	cases := map[string]bool{
		"internal/model/feed_test.go":    true,
		"src/app.spec.tsx":               true,
		"src/app.test.mjs":               true,
		"tests/test_api.py":              true,
		"pkg/parse_test.py":              true,
		"spec/models/user_spec.rb":       true,
		"Tests/AppTests/FooTests.swift":  true,
		"src/FooTest.php":                true,
		"app/tests.py":                   true,
		"base/strings_unittest.cc":       true,
		"src/FooIT.java":                 true,
		"src/FooSpec.kt":                 true,
		"cypress/e2e/login.cy.ts":        true,
		"src/__mocks__/api.js":           true,
		"Foo.Tests/Bar.cs":               true,
		"api/spec/openapi.yaml":          false,
		"src/test/java/FooTest.java":     true,
		"app/FooTests.cs":                true,
		"internal/reader/testdata/a.xml": true,
		"fixtures/users.json":            true,
		"__snapshots__/a.snap":           true,
		"internal/model/feed.go":         false,
		"src/latest.ts":                  false,
		"src/attestation.go":             false,
		"docs/testing-guide.md":          false,
		"src/Contest.java":               false,
	}
	for path, want := range cases {
		if got := IsTest(path); got != want {
			t.Errorf("IsTest(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestGeneratedFilesAreSkippedUnlessNamed(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		"app.js":                     "x\n",
		"package-lock.json":          "{}\n",
		"web/pnpm-lock.yaml":         "x\n",
		"yarn.lock":                  "x\n",
		"go.sum":                     "x\n",
		"Cargo.lock":                 "x\n",
		"dist/app.js.map":            "{}\n",
		"vendor/jquery.min.js":       "x\n",
		"logo.svg":                   "<svg/>\n",
		"node_modules/dep/a.js":      "x\n",
		"web/node_modules/b.js":      "x\n",
		"node_modules/dep/test/a.js": "x\n", // generated, not a test: Options.Tests wouldn't bring it back
		"ICON.SVG":                   "<svg/>\n",
		"src/sitemap.go":             "package src\n", // .map only as a suffix of a source map
		"src/lockfile.go":            "package src\n",
		"src/admin.js":               "x\n",
		"src/node_modules_doc.go":    "package src\n",
	})
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app.js", "src/admin.js", "src/lockfile.go", "src/node_modules_doc.go", "src/sitemap.go"}
	if got := paths(res.Files); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if res.Generated != 12 || res.Tests != 0 {
		t.Fatalf("want 12 generated files and no tests counted as skipped, got %d and %d", res.Generated, res.Tests)
	}

	res, _ = Walk([]string{"package-lock.json", "node_modules/dep"}, Options{Tests: true})
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"node_modules/dep/a.js", "node_modules/dep/test/a.js", "package-lock.json"}) {
		t.Fatalf("generated files named explicitly are searched: %v", got)
	}
}

func TestDocsAreSkippedUnlessAsked(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		"main.go":            "package main\n",
		"README.md":          "x\n",
		"docs/guide.mdx":     "x\n",
		"docs/api.rst":       "x\n",
		"CHANGELOG.markdown": "x\n",
		"requirements.txt":   "x\n",
	})
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"main.go", "requirements.txt"}) {
		t.Fatalf("files %v", got)
	}
	if res.Docs != 4 {
		t.Fatalf("want 4 docs counted as skipped, got %d", res.Docs)
	}

	res, _ = Walk([]string{"."}, Options{Docs: true})
	if len(res.Files) != 6 || res.Docs != 0 {
		t.Fatalf("Docs should include all 6 files, got %v (skipped %d)", paths(res.Files), res.Docs)
	}

	res, _ = Walk([]string{"README.md"}, Options{})
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"README.md"}) {
		t.Fatalf("a doc named explicitly is searched: %v", got)
	}
}

func TestWorktreesShareTheMainCheckoutAsRepo(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	tree(t, main, map[string]string{"src/app.go": "package app\n"})
	git(t, main, "init", "-q")
	git(t, main, "add", ".")
	git(t, main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
	git(t, main, "worktree", "add", "-q", filepath.Join(dir, "wt"))
	main, _ = filepath.EvalSymlinks(main)
	wt, _ := filepath.EvalSymlinks(filepath.Join(dir, "wt"))

	for _, c := range []struct{ in, root string }{{main, main}, {filepath.Join(wt, "src"), wt}} {
		res, err := Walk([]string{c.in}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Root != c.root || res.Repo != main {
			t.Errorf("walking %s: root %q repo %q, want root %q repo %q", c.in, res.Root, res.Repo, c.root, main)
		}
		if got := rels(res.Files); !reflect.DeepEqual(got, []string{"src/app.go"}) {
			t.Errorf("walking %s: paths are relative to the worktree, got %v", c.in, got)
		}
		if repo, _ := Repo(c.in); repo != main {
			t.Errorf("Repo(%s) = %q, want %q", c.in, repo, main)
		}
	}

	plain := t.TempDir()
	tree(t, plain, map[string]string{"a.go": "package a\n"})
	res, _ := Walk([]string{plain}, Options{})
	if res.Repo != res.Root {
		t.Errorf("outside git the repo is the root: %q vs %q", res.Repo, res.Root)
	}
}

func TestTranslationsAreSkippedUnlessNamed(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, map[string]string{
		"main.go":                              "package main\n",
		"internal/locale/translations/de.json": "{}\n",
		"internal/locale/catalog.go":           "package locale\n", // code that loads them is searched
		"config/locales/fr.yml":                "fr:\n",
		"src/i18n/es.json":                     "{}\n",
		"app/l10n/ja.toml":                     "x\n",
		"res/Lang/pt.properties":               "x\n",
		"po/nl.po":                             "msgid \"\"\n",
		"messages.pot":                         "msgid \"\"\n",
		"lib/l10n/app_en.arb":                  "{}\n",
		"ios/en.lproj/Localizable.strings":     "\"a\" = \"b\";\n",
		"web/xlf/messages.fr.XLF":              "<xliff/>\n",
		"web/messages.de.xliff":                "<xliff/>\n",
		"src/language/parser.json":             "{}\n", // not a translation folder
		"config/settings.json":                 "{}\n",
	})
	t.Chdir(dir)

	res, err := Walk([]string{"."}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"config/settings.json", "internal/locale/catalog.go", "main.go", "src/language/parser.json"}
	if got := paths(res.Files); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if res.Translations != 11 {
		t.Fatalf("want 11 translation files counted as skipped, got %d", res.Translations)
	}

	res, _ = Walk([]string{"internal/locale/translations", "po/nl.po"}, Options{})
	if got := paths(res.Files); !reflect.DeepEqual(got, []string{"internal/locale/translations/de.json", "po/nl.po"}) {
		t.Fatalf("translations named explicitly are searched: %v", got)
	}
}
