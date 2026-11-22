package walk

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// benchTree writes a repo-shaped tree of about 3,000 files: mostly small
// source files, some larger ones, and binaries, tests, docs and generated
// files in the mix the classifiers see in practice.
func benchTree(b *testing.B, dir string) {
	b.Helper()
	small := []byte("package x\n\n" + strings.Repeat("func f() { println(\"hello\") }\n", 60))
	large := []byte(strings.Repeat("export function g(a: number) { return a * 2 }\n", 800))
	binary := append([]byte("\x89PNG\x00\x00"), make([]byte, 20<<10)...)
	write := func(name string, body []byte) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for pkg := range 100 {
		base := fmt.Sprintf("internal/pkg%02d/sub%d", pkg, pkg%7)
		for i := range 20 {
			write(fmt.Sprintf("%s/handler_%02d.go", base, i), small)
		}
		write(base+"/handler_test.go", small)
		write(base+"/README.md", small)
		write(fmt.Sprintf("web/src/mod%02d/view.ts", pkg), large)
		write(fmt.Sprintf("web/src/mod%02d/view.test.ts", pkg), large)
		write(fmt.Sprintf("web/public/img%02d.png", pkg), binary)
		write(fmt.Sprintf("web/public/icon%02d.svg", pkg), small)
		write(fmt.Sprintf("web/locales/lang%02d.json", pkg), small)
		write(fmt.Sprintf("build/out%02d.js", pkg), small)
	}
	write(".gitignore", []byte("/build\n*.log\n*.tmp\ncoverage/\n/dist\n.env\n*.pyc\n__pycache__/\nnode_modules/\n.DS_Store\n*.swp\n/tmp/**\n"))
	write("web/.gitignore", []byte("/dist\n*.local\n"))
}

func benchWalk(b *testing.B, useGit bool) {
	dir := b.TempDir()
	benchTree(b, dir)
	if useGit {
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git init: %v\n%s", err, out)
		}
	}
	b.Chdir(dir)
	b.ReportAllocs()
	for b.Loop() {
		res, err := Walk([]string{"."}, Options{})
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Files) != 2102 {
			b.Fatalf("got %d files", len(res.Files))
		}
	}
}

func BenchmarkWalkGit(b *testing.B)   { benchWalk(b, true) }
func BenchmarkWalkNoGit(b *testing.B) { benchWalk(b, false) }

// BenchmarkClassify runs the per-file path checks Walk makes for every
// listed file before opening any of them.
func BenchmarkClassify(b *testing.B) {
	rels := []string{
		"internal/model/feed_handler.go",
		"src/components/Button/Button.tsx",
		"web/src/app/routes/settings/profile.ts",
		"pkg/server/http/middleware/auth.go",
		"lib/core/utils/strings.py",
		"internal/model/feed_test.go",
		"web/src/app.test.ts",
		"docs/guide/install.md",
		"web/locales/de.json",
		"package-lock.json",
		"app/src/main/java/com/example/MainActivity.java",
		"crates/engine/src/lib.rs",
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, rel := range rels {
			_ = IsGenerated(rel) || IsTranslation(rel) || IsTest(rel) || IsDoc(rel)
		}
	}
}

// BenchmarkIgnored matches paths against a typical root .gitignore plus a
// nested one, as walkDir does outside git for every file and folder.
func BenchmarkIgnored(b *testing.B) {
	var rs *ruleSet
	for _, g := range []struct{ base, body string }{
		{"repo", "/build\n*.log\n*.tmp\ncoverage/\n/dist\n.env\n*.pyc\n__pycache__/\nnode_modules/\n.DS_Store\n*.swp\n/tmp/**\ndocs/**/draft.md\n"},
		{"repo/web", "/dist\n*.local\n"},
	} {
		next := &ruleSet{}
		if rs != nil {
			next.rules = append(next.rules, rs.rules...)
		}
		for line := range strings.Lines(g.body) {
			if r, ok := parseRule(g.base, line); ok {
				next.rules = append(next.rules, r)
			}
		}
		rs = next
	}
	paths := []string{
		"repo/web/src/app/routes/settings/profile.ts",
		"repo/web/src/components/Button.tsx",
		"repo/web/dist",
		"repo/web/debug.log",
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range paths {
			rs.ignored(p, false)
		}
	}
}
