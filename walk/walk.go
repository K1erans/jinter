// Package walk lists the files to search. Inside a git repo it asks git,
// so ignore rules match git exactly; elsewhere it reads .gitignore files.
package walk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// binarySniffLen is how much of a file is checked for a NUL byte.
const binarySniffLen = 8 << 10

type Options struct {
	NoIgnore    bool  // also list gitignored files
	Tests       bool  // also list test files and test data (see IsTest)
	Docs        bool  // also list documentation (see IsDoc)
	MaxFileSize int64 // skip larger files; 0 means no limit
}

type File struct {
	Path string // as shown to the user, relative to the working directory
	Rel  string // slash path from the repo root; stable across working directories
	Size int64

	// Src is the file's content, read while checking it is text, so it
	// isn't opened twice. Walk fills it for Result.Files only; nil means
	// read it from Path.
	Src []byte
}

type Result struct {
	Root      string // git top-level folder, or the first search path outside git
	Repo      string // the repository's main checkout, shared by its worktrees (see Repo)
	Files     []File // sorted by Path
	Oversize  []File // text files skipped for being over MaxFileSize
	Tests     int    // text files skipped as tests because Options.Tests was off
	Generated int    // files skipped as generated (see IsGenerated), text or not
	Docs      int    // text files skipped as docs because Options.Docs was off

	Translations int // files skipped as translation catalogs (see IsTranslation)
}

// Walk lists text files under paths. Named files are always included;
// directories follow .gitignore unless opts.NoIgnore is set, skip generated
// files and translations, and skip tests and docs unless opts.Tests or
// opts.Docs is set.
func Walk(paths []string, opts Options) (Result, error) {
	var res Result
	if len(paths) == 0 {
		paths = []string{"."}
	}
	root, err := Root(paths[0])
	if err != nil {
		return res, err
	}
	res.Root = root
	res.Repo = repoOf(root)

	// found is one file under the named path base. Only the part of its
	// path below base is checked for tests, generated files, translations
	// and docs, so naming such a file or folder searches it.
	type found struct {
		display, rel string
		test, doc    bool // would be skipped as a test or doc, unless included
	}
	var list []found
	seen := map[string]bool{}
	// baseRel is base's path from the root. Resolving it once per named
	// path, not once per file, saves a stat of every folder on the way to
	// every file.
	add := func(display, base, baseRel string) {
		display = filepath.Clean(display)
		if seen[display] {
			return
		}
		seen[display] = true
		below, err := filepath.Rel(base, display)
		if err != nil {
			below = "" // can't happen for paths found under base; checks nothing
		}
		below = filepath.ToSlash(below)
		// Generated files are dropped unopened: an unignored node_modules
		// can hold hundreds of thousands. A test among them is still
		// generated, so Options.Tests doesn't bring it back.
		if IsGenerated(below) {
			res.Generated++
			return
		}
		if IsTranslation(below) {
			res.Translations++
			return
		}
		list = append(list, found{
			display: display,
			rel:     path.Join(baseRel, below),
			test:    !opts.Tests && IsTest(below),
			doc:     !opts.Docs && IsDoc(below),
		})
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return res, err
		}
		baseRel := relTo(root, p)
		if !info.IsDir() {
			add(p, p, baseRel)
			continue
		}
		var names []string
		if gitTop(p) != "" && !opts.NoIgnore {
			names, err = gitFiles(p)
		} else {
			names, err = walkDir(p, !opts.NoIgnore)
		}
		if err != nil {
			return res, err
		}
		for _, n := range names {
			add(n, p, baseRel)
		}
	}

	// Opening every file to check it is text is most of a walk's time, and
	// spends it waiting on the file system, so it runs in parallel. Files
	// that will be searched are read whole while open.
	files := make([]File, len(list))
	isText := make([]bool, len(list))
	errs := make([]error, len(list))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(4*runtime.GOMAXPROCS(0), len(list)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				c := list[i]
				files[i], isText[i], errs[i] = inspect(c.display, !c.test && !c.doc, opts.MaxFileSize)
				files[i].Rel = c.rel
			}
		}()
	}
	for i := range list {
		next <- i
	}
	close(next)
	wg.Wait()

	for i, c := range list {
		if errs[i] != nil {
			return res, errs[i]
		}
		if !isText[i] {
			continue
		}
		f := files[i]
		switch {
		case c.test:
			res.Tests++
			continue
		case c.doc:
			res.Docs++
			continue
		}
		if oversize(f.Size, opts.MaxFileSize) {
			res.Oversize = append(res.Oversize, f)
		} else {
			res.Files = append(res.Files, f)
		}
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
	sort.Slice(res.Oversize, func(i, j int) bool { return res.Oversize[i].Path < res.Oversize[j].Path })
	return res, nil
}

// Root is the git top-level folder containing path, or path itself (its
// folder, for a file) outside git. Symlinks are resolved.
func Root(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	dir := path
	if !info.IsDir() {
		dir = filepath.Dir(path)
	}
	if top := gitTop(dir); top != "" {
		return top, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Repo names the repository holding path, the same for every worktree of
// it: the main checkout's folder, or Root outside git. Answers are keyed by
// repo-relative path and code, so worktrees can share one cache; giving each
// its own would make every new worktree pay for a full scan again.
func Repo(path string) (string, error) {
	root, err := Root(path)
	if err != nil {
		return "", err
	}
	return repoOf(root), nil
}

func repoOf(root string) string {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return root // not in git
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	// The usual layout is <main checkout>/.git; a bare repository or a
	// submodule keeps its git folder elsewhere and is named by it.
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
}

func gitTop(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		return resolved
	}
	return top
}

// gitFiles lists tracked and untracked-but-not-ignored files under dir.
func gitFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %v: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, filepath.Join(dir, filepath.FromSlash(name)))
		}
	}
	return files, nil
}

// walkDir lists every file under dir except .git folders, applying
// .gitignore files found along the way when ignore is set.
func walkDir(dir string, ignore bool) ([]string, error) {
	var files []string
	rules := map[string]*ruleSet{} // folder → rules in effect there
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable entries are skipped, as git does
		}
		parent := rules[filepath.Dir(p)]
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if p != dir && parent.ignored(p, true) {
				return filepath.SkipDir
			}
			rs := parent
			if ignore {
				rs = parent.with(p)
			}
			rules[p] = rs
			return nil
		}
		if !parent.ignored(p, false) {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

func oversize(size, limit int64) bool { return limit > 0 && size > limit }

// inspect stats a file and reports whether it is a searchable text file.
// With read set, a text file within maxSize (0 = no limit) is read whole
// into Src.
func inspect(path string, read bool, maxSize int64) (File, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil // e.g. tracked by git but deleted
	}
	if err != nil {
		return File{}, false, err
	}
	if !info.Mode().IsRegular() {
		return File{}, false, nil
	}
	fh, err := os.Open(path)
	if err != nil {
		return File{}, false, err
	}
	defer fh.Close()
	// A small file fits the head with a byte to spare, which shows it
	// hasn't grown since Stat, so the head is its whole content.
	head := make([]byte, min(binarySniffLen, info.Size()+1))
	n, err := io.ReadFull(fh, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return File{}, false, err
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return File{}, false, nil
	}
	f := File{Path: path, Size: info.Size()}
	if read && !oversize(f.Size, maxSize) {
		buf := bytes.NewBuffer(head[:n])
		if n == len(head) {
			buf.Grow(max(0, int(f.Size)-n) + bytes.MinRead)
			if _, err := buf.ReadFrom(fh); err != nil {
				return File{}, false, err
			}
		}
		f.Src = buf.Bytes()
	}
	return f, true, nil
}

func relTo(root, path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}
