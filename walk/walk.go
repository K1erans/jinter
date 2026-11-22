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
	root, repoDir, firstInGit, err := findRootAndRepo(paths[0])
	if err != nil {
		return res, err
	}
	res.Root = root
	res.Repo = repoDir

	// Fast streaming path: single git directory with ignore rules enabled.
	// Streams git ls-files stdout directly into the parallel file-inspection
	// pool so git listing and file reading overlap instead of running serially.
	if len(paths) == 1 && !opts.NoIgnore && firstInGit {
		info, err := os.Stat(paths[0])
		if err != nil {
			return res, err
		}
		if info.IsDir() {
			return walkSingleGitDir(paths[0], root, repoDir, opts)
		}
	}

	// found is one file under the named path base. Only the part of its
	// path below base is checked for tests, generated files, translations
	// and docs, so naming such a file or folder searches it.
	type found struct {
		display, rel string
		test, doc    bool // would be skipped as a test or doc, unless included
	}
	var list []found
	dedupePaths := len(paths) > 1
	var seen map[string]bool
	if dedupePaths {
		seen = make(map[string]bool, 64)
	}
	// named holds the files named in paths. Reading one is an error rather
	// than an Unreadable, as for a named path that is missing.
	named := map[string]bool{}
	// relOf is display's slash path below the named path base, and from the
	// root. baseRel is base's path from the root.
	relOf := func(display, base, baseRel string) (below, rel string) {
		if base == "." {
			below = filepath.ToSlash(display)
		} else if display == base {
			below = "."
		} else if rest, ok := cutPathPrefix(display, base); ok {
			below = filepath.ToSlash(rest)
		} else {
			var err error
			below, err = filepath.Rel(base, display)
			if err != nil {
				below = "" // can't happen for paths found under base; checks nothing
			}
			below = filepath.ToSlash(below)
		}
		rel = below // already clean, so joining onto "." would only copy it
		if baseRel != "." {
			if below == "." {
				rel = baseRel
			} else if below != "" {
				rel = baseRel + "/" + below
			} else {
				rel = path.Join(baseRel, below)
			}
		}
		return below, rel
	}
	add := func(display, base, baseRel string, alreadyClean bool) {
		if !alreadyClean {
			display = filepath.Clean(display)
		}
		if dedupePaths {
			if seen[display] {
				return
			}
			seen[display] = true
		}
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
		cleanBase := filepath.Clean(p)
		if !info.IsDir() {
			named[cleanBase] = true
			add(cleanBase, cleanBase, baseRel, true)
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
		if len(list) == 0 && len(names) > 64 {
			list = make([]found, 0, len(names))
			if dedupePaths {
				seen = make(map[string]bool, len(names))
			}
		}
		for _, n := range names {
			add(n, cleanBase, baseRel, true)
		}
		for _, f := range failed {
			display := filepath.Clean(f.path)
			if dedupePaths {
				if seen[display] {
					continue // also under another named folder
				}
				seen[display] = true
			}
			_, rel := relOf(display, cleanBase, baseRel)
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
	workers := min(2*runtime.GOMAXPROCS(0), len(list))
	const slabSize = 128 << 10
	const slabMaxFile = 32 << 10
	for range workers {
		wg.Go(func() {
			head := make([]byte, binarySniffLen)
			var slab []byte
			slabOff := 0
			for {
				i := int(next.Add(1) - 1)
				if i >= len(list) {
					return
				}
				c := list[i]
				files[i], isText[i], errs[i], slab, slabOff = inspectSlab(c.display, !c.test && !c.doc, opts.MaxFileSize, head, slab, slabOff, slabSize, slabMaxFile)
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

func cutPathPrefix(p, base string) (string, bool) {
	if len(p) > len(base) && p[len(base)] == filepath.Separator && p[:len(base)] == base {
		return p[len(base)+1:], true
	}
	return "", false
}

// walkSingleGitDir streams git ls-files stdout directly into the parallel
// file-inspection worker pool so listing and opening/reading overlap.
func walkSingleGitDir(dir, root, repoDir string, opts Options) (Result, error) {
	res := Result{Root: root, Repo: repoDir}
	baseRel := relTo(root, dir)
	cleanDir := filepath.Clean(dir)
	displayPrefix := ""
	if cleanDir != "." {
		displayPrefix = cleanDir + string(filepath.Separator)
	}
	relPrefix := ""
	if baseRel != "." {
		relPrefix = baseRel + "/"
	}

	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("git ls-files in %s: %w", dir, err)
	}

	type workItem struct {
		display, rel string
		test, doc    bool
	}
	const batchCap = 16
	maxWorkers := runtime.GOMAXPROCS(0)
	jobs := make(chan []workItem, maxWorkers*2)

	type workerResult struct {
		files      []File
		oversize   []File
		unreadable []Unreadable
		tests      int
		docs       int
	}
	wres := make([]workerResult, maxWorkers)
	const slabSize = 128 << 10
	const slabMaxFile = 32 << 10
	var wg sync.WaitGroup
	spawned := 0
	spawnWorker := func() {
		w := spawned
		spawned++
		wg.Go(func() {
			head := make([]byte, binarySniffLen)
			var slab []byte
			slabOff := 0
			var out workerResult
			for batch := range jobs {
				for _, c := range batch {
					var f File
					var isText bool
					var ferr error
					f, isText, ferr, slab, slabOff = inspectSlab(c.display, !c.test && !c.doc, opts.MaxFileSize, head, slab, slabOff, slabSize, slabMaxFile)
					if ferr != nil {
						out.unreadable = append(out.unreadable, Unreadable{Path: c.display, Rel: c.rel, Err: ferr})
						continue
					}
					if !isText {
						continue
					}
					f.Rel = c.rel
					switch {
					case c.test:
						out.tests++
					case c.doc:
						out.docs++
					case oversize(f.Size, opts.MaxFileSize):
						out.oversize = append(out.oversize, f)
					default:
						out.files = append(out.files, f)
					}
				}
			}
			wres[w] = out
		})
	}

	// Read NUL-delimited names from git stdout in 64 KiB chunks and dispatch
	// batches to workers immediately, spawning workers lazily up to maxWorkers.
	buf := make([]byte, 64<<10)
	var rem []byte
	batch := make([]workItem, 0, batchCap)
	flush := func() {
		if len(batch) > 0 {
			if spawned < maxWorkers {
				spawnWorker()
			}
			jobs <- batch
			batch = make([]workItem, 0, batchCap)
		}
	}
	handleBelow := func(below string) {
		if len(below) == 0 {
			return
		}
		if IsGenerated(below) {
			res.Generated++
			return
		}
		if IsTranslation(below) {
			res.Translations++
			return
		}
		display := below
		if filepath.Separator != '/' {
			display = filepath.FromSlash(display)
		}
		if displayPrefix != "" {
			display = displayPrefix + display
		}
		rel := below
		if relPrefix != "" {
			rel = relPrefix + below
		}
		batch = append(batch, workItem{
			display: display,
			rel:     rel,
			test:    !opts.Tests && IsTest(below),
			doc:     !opts.Docs && IsDoc(below),
		})
		if len(batch) == batchCap {
			flush()
		}
	}

	var readErr error
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if len(rem) > 0 {
				if idx := bytes.IndexByte(chunk, 0); idx >= 0 {
					rem = append(rem, chunk[:idx]...)
					handleBelow(string(rem))
					rem = rem[:0]
					chunk = chunk[idx+1:]
				} else {
					rem = append(rem, chunk...)
					chunk = nil
				}
			}
			if len(chunk) > 0 {
				lastNul := bytes.LastIndexByte(chunk, 0)
				if lastNul < 0 {
					rem = append(rem[:0], chunk...)
				} else {
					// Convert the whole completed slice into a single string slab
					// so every filename in this chunk is a zero-alloc substring.
					slabStr := string(chunk[:lastNul])
					start := 0
					for start < len(slabStr) {
						idx := strings.IndexByte(slabStr[start:], 0)
						if idx < 0 {
							handleBelow(slabStr[start:])
							break
						}
						handleBelow(slabStr[start : start+idx])
						start += idx + 1
					}
					if lastNul+1 < len(chunk) {
						rem = append(rem[:0], chunk[lastNul+1:]...)
					}
				}
			}
		}
		if rerr == io.EOF {
			if len(rem) > 0 {
				handleBelow(string(rem))
			}
			break
		}
		if rerr != nil {
			readErr = rerr
			break
		}
	}
	flush()
	close(jobs)
	wg.Wait()

	waitErr := cmd.Wait()
	if waitErr != nil {
		return Result{}, fmt.Errorf("git ls-files in %s: %w: %s", dir, waitErr, strings.TrimSpace(stderr.String()))
	}
	if readErr != nil {
		return Result{}, readErr
	}

	totalFiles, totalOversize, totalUnreadable := 0, 0, 0
	for i := range spawned {
		totalFiles += len(wres[i].files)
		totalOversize += len(wres[i].oversize)
		totalUnreadable += len(wres[i].unreadable)
		res.Tests += wres[i].tests
		res.Docs += wres[i].docs
	}
	if totalFiles > 0 {
		res.Files = make([]File, 0, totalFiles)
	}
	if totalOversize > 0 {
		res.Oversize = make([]File, 0, totalOversize)
	}
	if totalUnreadable > 0 {
		res.Unreadable = make([]Unreadable, 0, totalUnreadable)
	}
	for i := range spawned {
		res.Files = append(res.Files, wres[i].files...)
		res.Oversize = append(res.Oversize, wres[i].oversize...)
		res.Unreadable = append(res.Unreadable, wres[i].unreadable...)
	}
	for _, f := range unopenedDirs(dir, stderr.String()) {
		display := filepath.Clean(f.path)
		below := display
		if cleanDir != "." {
			if rest, ok := cutPathPrefix(display, cleanDir); ok {
				below = filepath.ToSlash(rest)
			} else if r, err := filepath.Rel(cleanDir, display); err == nil {
				below = filepath.ToSlash(r)
			}
		} else {
			below = filepath.ToSlash(display)
		}
		rel := below
		if baseRel != "." {
			if below == "." {
				rel = baseRel
			} else {
				rel = baseRel + "/" + below
			}
		}
		res.Unreadable = append(res.Unreadable, Unreadable{Path: display, Rel: rel, Err: f.err})
	}
	slices.SortFunc(res.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(res.Oversize, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
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
	root, _, _, err := findRootAndRepo(path)
	return root, err
}

// findRootAndRepo resolves Root and Repo in one pass without spawning git
// subprocesses for standard repositories and worktrees.
func findRootAndRepo(path string) (root, repo string, inGit bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "", false, err
	}
	dir := path
	if !info.IsDir() {
		dir = filepath.Dir(path)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", false, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", false, err
	}
	if top, common, ok := findGitDirect(resolved); ok {
		return top, common, true, nil
	}
	// Fall back to git CLI when GIT_DIR/GIT_WORK_TREE or unusual layouts apply.
	if os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_WORK_TREE") != "" {
		if top := gitTop(dir); top != "" {
			return top, repoOf(top), true, nil
		}
	}
	return resolved, resolved, false, nil
}

// findGitDirect walks upward from resolved looking for a .git folder or file
// (for worktrees/submodules) to determine the work tree root and main checkout
// without spawning git rev-parse.
func findGitDirect(resolved string) (top, repo string, ok bool) {
	if os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_WORK_TREE") != "" || os.Getenv("GIT_CEILING_DIRECTORIES") != "" {
		return "", "", false
	}
	cur := resolved
	for {
		dotGit := filepath.Join(cur, ".git")
		fi, err := os.Lstat(dotGit)
		if err == nil {
			if fi.IsDir() {
				// Standard repo: check it looks like a gitdir (HEAD exists).
				if _, err := os.Stat(filepath.Join(dotGit, "HEAD")); err == nil {
					return cur, cur, true
				}
			} else if fi.Mode().IsRegular() {
				// Worktree or submodule: ".git" file contains "gitdir: <path>".
				if data, err := os.ReadFile(dotGit); err == nil {
					line := strings.TrimSpace(string(data))
					if gd, hasPrefix := strings.CutPrefix(line, "gitdir:"); hasPrefix {
						gd = strings.TrimSpace(gd)
						if !filepath.IsAbs(gd) {
							gd = filepath.Join(cur, gd)
						}
						if r, err := filepath.EvalSymlinks(gd); err == nil {
							gd = r
						}
						common := gd
						if cd, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
							c := strings.TrimSpace(string(cd))
							if !filepath.IsAbs(c) {
								c = filepath.Join(gd, c)
							}
							if r, err := filepath.EvalSymlinks(c); err == nil {
								common = r
							} else {
								common = filepath.Clean(c)
							}
						}
						if filepath.Base(common) == ".git" {
							return cur, filepath.Dir(common), true
						}
						return cur, common, true
					}
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", false
		}
		cur = parent
	}
}

// Repo names the repository holding path, the same for every worktree of
// it: the main checkout's folder, or Root outside git. Answers are keyed by
// repo-relative path and code, so worktrees can share one cache; giving each
// its own would make every new worktree pay for a full scan again.
func Repo(path string) (string, error) {
	_, repo, _, err := findRootAndRepo(path)
	return repo, err
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
	if abs, err := filepath.Abs(dir); err == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			if top, _, ok := findGitDirect(resolved); ok {
				return top
			}
		}
	}
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
	cleanDir := filepath.Clean(dir)
	prefix := ""
	if cleanDir != "." {
		prefix = cleanDir + string(filepath.Separator)
	}
	files := make([]string, 0, bytes.Count(out, []byte{0}))
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name == "" {
			continue
		}
		if filepath.Separator != '/' {
			name = filepath.FromSlash(name)
		}
		if prefix != "" {
			name = prefix + name
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
//
// Uses os.ReadDir directly so it only reads .gitignore in folders that
// actually contain one, and walks subtrees in parallel.
func walkDir(dir string, ignore bool) ([]string, []failure, error) {
	dir = filepath.Clean(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var rootRules *ruleSet
	if ignore && hasEntry(entries, ".gitignore") {
		rootRules = rootRules.with(dir)
	}

	type dirJob struct {
		path  string
		rules *ruleSet
	}
	var files []string
	var failed []failure
	var subdirs []dirJob

	join := func(parent, name string) string {
		if parent == "." {
			return name
		}
		return parent + string(filepath.Separator) + name
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == ".git" {
				continue
			}
			p := join(dir, name)
			if ignore && rootRules.ignored(p, true) {
				continue
			}
			subdirs = append(subdirs, dirJob{path: p, rules: rootRules})
			continue
		}
		p := join(dir, name)
		if !ignore || !rootRules.ignored(p, false) {
			files = append(files, p)
		}
	}

	if len(subdirs) == 0 {
		return files, failed, nil
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var next atomic.Int64
	workers := min(runtime.GOMAXPROCS(0), len(subdirs))
	for range workers {
		wg.Go(func() {
			var localFiles []string
			var localFailed []failure
			var stack []dirJob
			for {
				idx := int(next.Add(1) - 1)
				if idx >= len(subdirs) {
					break
				}
				stack = append(stack[:0], subdirs[idx])
				for len(stack) > 0 {
					job := stack[len(stack)-1]
					stack = stack[:len(stack)-1]

					ents, err := os.ReadDir(job.path)
					if err != nil {
						if !errors.Is(err, fs.ErrNotExist) {
							localFailed = append(localFailed, failure{path: job.path, err: err})
						}
						continue
					}
					rs := job.rules
					if ignore && hasEntry(ents, ".gitignore") {
						rs = rs.with(job.path)
					}
					for _, e := range ents {
						name := e.Name()
						if e.IsDir() {
							if name == ".git" {
								continue
							}
							p := join(job.path, name)
							if ignore && rs.ignored(p, true) {
								continue
							}
							stack = append(stack, dirJob{path: p, rules: rs})
							continue
						}
						p := join(job.path, name)
						if !ignore || !rs.ignored(p, false) {
							localFiles = append(localFiles, p)
						}
					}
				}
			}
			if len(localFiles) > 0 || len(localFailed) > 0 {
				mu.Lock()
				files = append(files, localFiles...)
				failed = append(failed, localFailed...)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return files, failed, nil
}

func hasEntry(entries []os.DirEntry, name string) bool {
	// os.ReadDir returns entries sorted by filename.
	_, found := slices.BinarySearchFunc(entries, name, func(e os.DirEntry, target string) int {
		return strings.Compare(e.Name(), target)
	})
	return found
}

func oversize(size, limit int64) bool { return limit > 0 && size > limit }

// inspect stats a file and reports whether it is a searchable text file.
// Kept for direct unit tests; Walk uses inspectSlab for slab-allocated reads.
func inspect(path string, read bool, maxSize int64, head []byte) (File, bool, error) {
	f, isText, err, _, _ := inspectSlab(path, read, maxSize, head, nil, 0, 0, 0)
	return f, isText, err
}

// inspectSlab opens path once (using fstat instead of a separate path Stat),
// checks it is a regular text file, and reads small kept files directly into
// the worker's reusable slab to avoid per-file allocations and head copies.
func inspectSlab(path string, read bool, maxSize int64, head, slab []byte, slabOff, slabSize, slabMaxFile int) (File, bool, error, []byte, int) {
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return File{}, false, nil, slab, slabOff
		}
		// An unreadable directory (e.g. chmod 0) must still be skipped as
		// non-regular rather than reported as an unreadable file.
		if info, serr := os.Stat(path); serr == nil && !info.Mode().IsRegular() {
			return File{}, false, nil, slab, slabOff
		}
		return File{}, false, err, slab, slabOff
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return File{}, false, err, slab, slabOff
	}
	if !info.Mode().IsRegular() {
		return File{}, false, nil, slab, slabOff
	}
	size := info.Size()
	wantRead := read && !oversize(size, maxSize)

	// Fast path: read kept files directly into a slab (or right-sized buffer)
	// without copying through head first. For local regular files, a single
	// Read(size+1) returns n == size when the file hasn't grown, avoiding a
	// second EOF read() syscall per file.
	if wantRead && size <= int64(slabMaxFile) && slabSize > 0 {
		if size == 0 {
			return File{Path: path, Size: 0, Src: []byte{}}, true, nil, slab, slabOff
		}
		need := int(size) + 1
		if len(slab)-slabOff < need {
			slab = make([]byte, slabSize)
			slabOff = 0
		}
		buf := slab[slabOff : slabOff+need]
		n, err := fh.Read(buf)
		if err != nil && err != io.EOF {
			return File{}, false, err, slab, slabOff
		}
		if err != io.EOF && n < int(size) {
			m, rerr := io.ReadFull(fh, buf[n:])
			n += m
			if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
				return File{}, false, rerr, slab, slabOff
			}
		}
		sniff := min(n, binarySniffLen)
		if bytes.IndexByte(buf[:sniff], 0) >= 0 {
			return File{}, false, nil, slab, slabOff
		}
		if n < len(buf) {
			// File fit in slab without growing; commit slice with tight cap.
			src := slab[slabOff : slabOff+n : slabOff+n]
			slabOff += n
			return File{Path: path, Size: size, Src: src}, true, nil, slab, slabOff
		}
		// File grew since Stat; copy to standalone buffer and finish reading.
		src := make([]byte, n, n*2)
		copy(src, buf[:n])
		for {
			if len(src) == cap(src) {
				src = append(src, 0)[:len(src)]
			}
			m, err := fh.Read(src[len(src):cap(src)])
			src = src[:len(src)+m]
			if err == io.EOF {
				break
			}
			if err != nil {
				return File{}, false, err, slab, slabOff
			}
		}
		return File{Path: path, Size: size, Src: src}, true, nil, slab, slabOff
	}

	// Non-slab path: sniff head first (for skipped, oversize, or large files).
	if size == 0 {
		f := File{Path: path, Size: 0}
		if wantRead {
			f.Src = []byte{}
		}
		return f, true, nil, slab, slabOff
	}
	wantHead := min(int64(len(head)), size+1)
	h := head[:wantHead]
	n, err := fh.Read(h)
	if err != nil && err != io.EOF {
		return File{}, false, err, slab, slabOff
	}
	if err != io.EOF && n < int(min(int64(len(head)), size)) {
		m, rerr := io.ReadFull(fh, h[n:])
		n += m
		if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
			return File{}, false, rerr, slab, slabOff
		}
	}
	if bytes.IndexByte(h[:n], 0) >= 0 {
		return File{}, false, nil, slab, slabOff
	}
	f := File{Path: path, Size: size}
	if !wantRead {
		return f, true, nil, slab, slabOff
	}
	src := make([]byte, n, max(n, int(size))+1)
	copy(src, h[:n])
	if n == len(h) {
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
				return File{}, false, err, slab, slabOff
			}
		}
	}
	f.Src = src
	return f, true, nil, slab, slabOff
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
