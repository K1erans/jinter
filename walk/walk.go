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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// Unreadable is a file or folder found under a named folder that couldn't
// be read, such as one without read permission. It is left out and the
// walk carries on.
type Unreadable struct {
	Path string // as shown to the user, like File.Path
	Rel  string // slash path from the repo root, like File.Rel
	Err  error  // why it couldn't be read
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

	// Unreadable are the files and folders skipped because they couldn't be
	// read, sorted by Path. Whether they were tests, docs or text is unknown,
	// so they aren't counted anywhere else.
	Unreadable []Unreadable
}

// Walk lists text files under paths. Named files are always included;
// directories follow .gitignore unless opts.NoIgnore is set, skip generated
// files and translations, and skip tests and docs unless opts.Tests or
// opts.Docs is set.
//
// A file or folder found inside a named folder that can't be read goes in
// Result.Unreadable, and the walk carries on. A named path that is missing
// or can't be read, or a git command that fails, is an error.
func Walk(paths []string, opts Options) (res Result, err error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	root, firstInGit, err := findRoot(paths[0])
	if err != nil {
		return res, err
	}
	res.Root = root
	// Finding the main checkout is a git call of its own; it runs while
	// the files are listed and inspected.
	repo := make(chan string, 1)
	go func() { repo <- repoOf(root) }()
	defer func() { res.Repo = <-repo }()

	// found is one file under the named path base. Only the part of its
	// path below base is checked for tests, generated files, translations
	// and docs, so naming such a file or folder searches it.
	type found struct {
		display, rel string
		test, doc    bool // would be skipped as a test or doc, unless included
	}
	var list []found
	seen := map[string]bool{}
	// named holds the files named in paths. Reading one is an error rather
	// than an Unreadable, as for a named path that is missing.
	named := map[string]bool{}
	// relOf is display's slash path below the named path base, and from the
	// root. baseRel is base's path from the root. Resolving it once per
	// named path, not once per file, saves a stat of every folder on the way
	// to every file.
	relOf := func(display, base, baseRel string) (below, rel string) {
		below, err := filepath.Rel(base, display)
		if err != nil {
			below = "" // can't happen for paths found under base; checks nothing
		}
		below = filepath.ToSlash(below)
		rel = below // already clean, so joining onto "." would only copy it
		if baseRel != "." {
			rel = path.Join(baseRel, below)
		}
		return below, rel
	}
	add := func(display, base, baseRel string) {
		display = filepath.Clean(display)
		if seen[display] {
			return
		}
		seen[display] = true
		below, rel := relOf(display, base, baseRel)
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
			rel:     rel,
			test:    !opts.Tests && IsTest(below),
			doc:     !opts.Docs && IsDoc(below),
		})
	}
	ignore := !opts.NoIgnore
	for i, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return res, err
		}
		baseRel := relTo(root, p)
		if !info.IsDir() {
			named[filepath.Clean(p)] = true
			add(p, p, baseRel)
			continue
		}
		// Git is only asked when its ignore rules are wanted, and the first
		// path's answer came with the root.
		var names []string
		var failed []failure
		if ignore && (i == 0 && firstInGit || i > 0 && gitTop(p) != "") {
			names, failed, err = gitFiles(p)
		} else {
			names, failed, err = walkDir(p, ignore)
		}
		if err != nil {
			return res, err
		}
		for _, n := range names {
			add(n, p, baseRel)
		}
		for _, f := range failed {
			display := filepath.Clean(f.path)
			if seen[display] {
				continue // also under another named folder
			}
			seen[display] = true
			_, rel := relOf(display, p, baseRel)
			res.Unreadable = append(res.Unreadable, Unreadable{Path: display, Rel: rel, Err: f.err})
		}
	}
	// Sorting the list, not the results, keeps Files and Oversize in order.
	// Git and WalkDir have listed it nearly in order already.
	slices.SortFunc(list, func(a, b found) int { return strings.Compare(a.display, b.display) })

	// Opening every file to check it is text is most of a walk's time, and
	// spends it waiting on the file system, so it runs in parallel. Files
	// that will be searched are read whole while open.
	files := make([]File, len(list))
	isText := make([]bool, len(list))
	errs := make([]error, len(list))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(4*runtime.GOMAXPROCS(0), len(list)) {
		wg.Go(func() {
			head := make([]byte, binarySniffLen)
			for {
				i := int(next.Add(1) - 1)
				if i >= len(list) {
					return
				}
				c := list[i]
				files[i], isText[i], errs[i] = inspect(c.display, !c.test && !c.doc, opts.MaxFileSize, head)
				files[i].Rel = c.rel
			}
		})
	}
	wg.Wait()

	for i, c := range list {
		if errs[i] != nil {
			if named[c.display] {
				return res, errs[i]
			}
			res.Unreadable = append(res.Unreadable, Unreadable{Path: c.display, Rel: c.rel, Err: errs[i]})
			continue
		}
		if !isText[i] {
			continue
		}
		f := files[i]
		switch {
		case c.test:
			res.Tests++
		case c.doc:
			res.Docs++
		case oversize(f.Size, opts.MaxFileSize):
			res.Oversize = append(res.Oversize, f)
		default:
			res.Files = append(res.Files, f)
		}
	}
	// Folders the listing couldn't read came first; files from the sorted
	// list follow, so the two need merging.
	slices.SortFunc(res.Unreadable, func(a, b Unreadable) int { return strings.Compare(a.Path, b.Path) })
	return res, nil
}

// failure is a path a listing found but couldn't read.
type failure struct {
	path string
	err  error
}

// Root is the git top-level folder containing path, or path itself (its
// folder, for a file) outside git. Symlinks are resolved.
func Root(path string) (string, error) {
	root, _, err := findRoot(path)
	return root, err
}

// findRoot is Root, also reporting whether it found a git work tree, so
// Walk needn't ask git again when it lists path.
func findRoot(path string) (root string, inGit bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, err
	}
	dir := path
	if !info.IsDir() {
		dir = filepath.Dir(path)
	}
	if top := gitTop(dir); top != "" {
		return top, true, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	root, err = filepath.EvalSymlinks(abs)
	return root, false, err
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

// gitFiles lists tracked and untracked-but-not-ignored files under dir, and
// the folders git couldn't open to look for untracked files in.
func gitFiles(dir string) ([]string, []failure, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = dir
	// Untranslated, so the warnings about unreadable folders can be found.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("git ls-files in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	files := make([]string, 0, bytes.Count(out, []byte{0}))
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name == "" {
			continue
		}
		// Git's paths are clean, so under "." they need no joining.
		name = filepath.FromSlash(name)
		if dir != "." {
			name = filepath.Join(dir, name)
		}
		files = append(files, name)
	}
	return files, unopenedDirs(dir, stderr.String()), nil
}

// gitWarnPrefix starts the warning git prints, and carries on, for a folder
// it can't open while looking for untracked files.
const gitWarnPrefix = "warning: could not open directory '"

// unopenedDirs finds the folders git warned it couldn't open in the
// stderr of a git ls-files run in dir.
func unopenedDirs(dir, stderr string) []failure {
	if !strings.Contains(stderr, gitWarnPrefix) {
		return nil
	}
	// Git names them from the top of the work tree, not from dir.
	cmd := exec.Command("git", "rev-parse", "--show-prefix")
	cmd.Dir = dir
	out, _ := cmd.Output()
	prefix := strings.TrimSpace(string(out))
	var failed []failure
	for line := range strings.Lines(stderr) {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), gitWarnPrefix)
		if !ok {
			continue
		}
		end := strings.LastIndex(rest, "': ")
		if end < 0 {
			continue
		}
		name, reason := rest[:end], rest[end+len("': "):]
		name = strings.TrimSuffix(strings.TrimPrefix(name, prefix), "/")
		p := filepath.Join(dir, filepath.FromSlash(name))
		// Opening it again gives an error that errors.Is can test, such as
		// fs.ErrPermission; git's words stand in if that now works.
		fh, err := os.Open(p)
		if err == nil {
			fh.Close()
			err = &fs.PathError{Op: "open", Path: p, Err: errors.New(reason)}
		} else if errors.Is(err, fs.ErrNotExist) {
			continue // deleted since git looked, as for a file
		}
		failed = append(failed, failure{path: p, err: err})
	}
	return failed
}

// walkDir lists every file under dir except .git folders, applying
// .gitignore files found along the way when ignore is set, and the folders
// under dir it couldn't read. dir itself being unreadable is an error.
func walkDir(dir string, ignore bool) ([]string, []failure, error) {
	// WalkDir joins names onto dir with filepath.Join, which cleans, so
	// under a dir of "src/" or "./src" it would visit "src/a" and look for
	// rules under "src", missing the ones recorded for dir.
	dir = filepath.Clean(dir)
	var files []string
	var failed []failure
	rules := map[string]*ruleSet{} // folder → rules in effect there
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			if !errors.Is(err, fs.ErrNotExist) { // vanished since listed
				failed = append(failed, failure{path: p, err: err})
			}
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !ignore {
			if !d.IsDir() {
				files = append(files, p)
			}
			return nil
		}
		parent := rules[filepath.Dir(p)]
		if d.IsDir() {
			if p != dir && parent.ignored(p, true) {
				return filepath.SkipDir
			}
			rules[p] = parent.with(p)
			return nil
		}
		if !parent.ignored(p, false) {
			files = append(files, p)
		}
		return nil
	})
	return files, failed, err
}

func oversize(size, limit int64) bool { return limit > 0 && size > limit }

// inspect stats a file and reports whether it is a searchable text file.
// With read set, a text file within maxSize (0 = no limit) is read whole
// into Src. head is scratch space of binarySniffLen bytes for the check,
// so only files that are kept cost an allocation.
func inspect(path string, read bool, maxSize int64, head []byte) (File, bool, error) {
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
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil // deleted since Stat
	}
	if err != nil {
		return File{}, false, err
	}
	defer fh.Close()
	size := info.Size()
	// A small file fits the head with a byte to spare, which shows it
	// hasn't grown since Stat, so the head is its whole content.
	head = head[:min(int64(len(head)), size+1)]
	n, err := io.ReadFull(fh, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return File{}, false, err
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return File{}, false, nil
	}
	f := File{Path: path, Size: size}
	if !read || oversize(size, maxSize) {
		return f, true, nil
	}
	// Sized from Stat with a byte to spare, so reading to EOF normally
	// fits without growing; non-nil even for an empty file.
	src := make([]byte, n, max(n, int(size))+1)
	copy(src, head)
	if n == len(head) {
		for {
			if len(src) == cap(src) {
				src = append(src, 0)[:len(src)] // grown since Stat
			}
			m, err := fh.Read(src[len(src):cap(src)])
			src = src[:len(src)+m]
			if err == io.EOF {
				break
			}
			if err != nil {
				return File{}, false, err
			}
		}
	}
	f.Src = src
	return f, true, nil
}

// relTo is path's slash path from root, or path itself if it is outside.
func relTo(root, path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if rel, err := filepath.Rel(root, abs); err == nil && !escapes(rel) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

// escapes reports whether a relative path from filepath.Rel leads out of
// its base. A name that merely starts with dots, like "..env", doesn't.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
