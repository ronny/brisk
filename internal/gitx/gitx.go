// Package gitx runs git and parses its output. brisk needs only the git
// binary; it never changes the repository, its index, or its config.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ronny/brisk/risk"
)

// Repo is a git working tree.
type Repo struct {
	Root string
	Env  []string // extra environment, e.g. GIT_INDEX_FILE
}

// Open finds the top of the working tree that contains dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	out, err := (&Repo{Root: dir}).Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	return &Repo{Root: strings.TrimSpace(string(out))}, nil
}

// Run runs git with args and returns stdout.
//
// Git reads no attributes from the repository's trees or working tree:
// the change being assessed must not decide how it is read. A candidate
// .gitattributes could otherwise mark source as binary (-diff) to hide it,
// or bind a configured clean filter, which `git add` would run. Attributes
// come only from $GIT_DIR/info/attributes. fsmonitor hooks are off too.
func (r *Repo) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.RunInput(ctx, nil, args...)
}

// RunInput is Run with stdin.
func (r *Repo) RunInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	safe := []string{"-c", "core.quotePath=false", "-c", "color.ui=false",
		"-c", "core.attributesFile=" + os.DevNull, "-c", "core.fsmonitor=false"}
	if tree, err := emptyTree(ctx, r.Root); err != nil {
		return nil, err
	} else if tree != "" {
		safe = append(safe, "--attr-source="+tree)
	}
	return r.run(ctx, stdin, append(safe, args...)...)
}

func (r *Repo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Root
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = append(append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"), r.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

var emptyTrees sync.Map // repository root → empty tree ID for its hash

// emptyTree returns the empty tree ID in root's object format, or "" when
// root is not inside a repository (git then reads no tree attributes).
func emptyTree(ctx context.Context, root string) (string, error) {
	if v, ok := emptyTrees.Load(root); ok {
		return v.(string), nil
	}
	out, err := (&Repo{Root: root}).run(ctx, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return "", nil // not a repository; the real command reports it
	}
	var tree string
	switch f := strings.TrimSpace(string(out)); f {
	case "sha1":
		tree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	case "sha256":
		tree = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
	default:
		return "", fmt.Errorf("unsupported git object format %q", f)
	}
	emptyTrees.Store(root, tree)
	return tree, nil
}

// RevParse resolves a revision to a commit ID.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("unknown revision %q", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultBase returns the merge base of HEAD with origin/HEAD, main, or
// master, whichever exists first.
func (r *Repo) DefaultBase(ctx context.Context) (string, error) {
	for _, ref := range []string{"origin/HEAD", "origin/main", "main", "origin/master", "master"} {
		if _, err := r.RevParse(ctx, ref); err != nil {
			continue
		}
		out, err := r.Run(ctx, "merge-base", "HEAD", ref)
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("no --base given and none of origin/HEAD, main, master exist")
}

// MaxContent is the largest file brisk loads for language analysis.
const MaxContent = 1 << 20

// Changes returns the change from base to head. When head is "", it
// returns the change from base to the working tree, including untracked
// files that are not ignored. It uses a temporary index so the user's index
// is left alone.
func (r *Repo) Changes(ctx context.Context, base, head string) (*risk.ChangeSet, error) {
	baseID, err := r.RevParse(ctx, base)
	if err != nil {
		return nil, err
	}
	cs := &risk.ChangeSet{RepoRoot: r.Root, Base: baseID}
	diffArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "-M", "-U3", "--src-prefix=a/", "--dst-prefix=b/"}
	work := r
	if head != "" {
		headID, err := r.RevParse(ctx, head)
		if err != nil {
			return nil, err
		}
		cs.Head = headID
		diffArgs = append(diffArgs, baseID, headID)
	} else {
		idx, cleanup, err := r.tempIndex(ctx)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		// git add writes blobs; send them to a scratch object directory
		// so the repository is never written to.
		objs, err := r.Run(ctx, "rev-parse", "--path-format=absolute", "--git-path", "objects")
		if err != nil {
			return nil, err
		}
		scratch, err := os.MkdirTemp("", "brisk-objects-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(scratch)
		work = &Repo{Root: r.Root, Env: append(append([]string{}, r.Env...),
			"GIT_INDEX_FILE="+idx,
			"GIT_OBJECT_DIRECTORY="+scratch,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES="+strings.TrimSpace(string(objs)),
		)}
		if _, err := work.Run(ctx, "-c", "core.splitIndex=false", "add", "-A", "--", "."); err != nil {
			return nil, err
		}
		hidden, code, err := work.hiddenByChange(ctx, baseID)
		if err != nil {
			return nil, err
		}
		if len(code) > 0 {
			args := append([]string{"--literal-pathspecs", "-c", "core.splitIndex=false", "add", "-f", "--"}, code...)
			if _, err := work.Run(ctx, args...); err != nil {
				return nil, err
			}
		}
		cs.IgnoredByChange = hidden
		diffArgs = append(diffArgs, "--cached", baseID)
	}
	raw, err := work.Run(ctx, diffArgs...)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	cs.DiffHash = "sha256:" + hex.EncodeToString(sum[:])
	files, err := ParseDiff(raw)
	if err != nil {
		return nil, err
	}
	if err := r.loadContents(ctx, baseID, cs.Head, files); err != nil {
		return nil, err
	}
	cs.Files = files
	return cs, nil
}

// MaxHidden caps how many hidden paths hiddenByChange reports and how
// many code files it adds.
const MaxHidden = 50

// maxHiddenVisits caps the directory entries hiddenByChange walks.
const maxHiddenVisits = 10000

// hiddenByChange finds untracked files that the working tree's ignore
// rules hide but the base commit's rules do not, so a change cannot hide
// new code from its own assessment with a .gitignore it adds, edits, or
// that ignores itself. It returns the hidden paths (a directory, ending in
// "/", when the walk stopped early) and the code files among them to
// assess as added files. Directories that hold dependencies or build
// output are expected to be ignored and are left out. w must use the
// scratch index that already holds the change.
func (w *Repo) hiddenByChange(ctx context.Context, base string) (hidden, code []string, err error) {
	out, err := w.Run(ctx, "ls-files", "-z", "-o", "-i", "--exclude-standard", "--directory", "--no-empty-directory")
	if err != nil || len(out) == 0 {
		return nil, nil, err
	}
	var entries []string
	for _, e := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if !expectedIgnored(w.Root, e) {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}
	baseRules, cleanup, err := w.baseIgnoreRules(ctx, base)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	ignoredAtBase, err := checkIgnore(ctx, baseRules, entries)
	if err != nil {
		return nil, nil, err
	}
	var files []string
	budget := maxHiddenVisits
	for _, e := range entries {
		if ignoredAtBase[e] {
			continue
		}
		if !strings.HasSuffix(e, "/") {
			files = append(files, e)
			continue
		}
		found, complete := walkFiles(w.Root, e, &budget)
		files = append(files, found...)
		if !complete {
			hidden = append(hidden, e)
		}
	}
	// A file inside a newly hidden directory may already be ignored at
	// base by a rule of its own, such as *.log.
	ignoredAtBase, err = checkIgnore(ctx, baseRules, files)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		if ignoredAtBase[f] {
			continue
		}
		if len(hidden) < MaxHidden {
			hidden = append(hidden, f)
		}
		if len(code) < MaxHidden && isCode(filepath.Join(w.Root, filepath.FromSlash(f))) {
			code = append(code, f)
		}
	}
	return hidden, code, nil
}

// baseIgnoreRules makes a scratch repository whose ignore rules are the
// base commit's .gitignore files plus the real info/exclude, so git's own
// matcher can say what the base would have ignored.
func (w *Repo) baseIgnoreRules(ctx context.Context, base string) (*Repo, func(), error) {
	tmp, err := os.MkdirTemp("", "brisk-ignore-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	fail := func(err error) (*Repo, func(), error) { cleanup(); return nil, nil, err }
	rules := &Repo{Root: tmp}
	if _, err := rules.run(ctx, nil, "init", "-q"); err != nil {
		return fail(err)
	}
	list, err := w.Run(ctx, "ls-tree", "-r", "-z", base)
	if err != nil {
		return fail(err)
	}
	var paths, oids []string
	for _, e := range strings.Split(strings.TrimSuffix(string(list), "\x00"), "\x00") {
		// <mode> SP <type> SP <oid> TAB <path>
		meta, p, ok := strings.Cut(e, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && f[1] == "blob" && path.Base(p) == ".gitignore" {
			paths, oids = append(paths, p), append(oids, f[2])
		}
	}
	if len(oids) > 0 {
		out, err := w.RunInput(ctx, []byte(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch")
		if err != nil {
			return fail(err)
		}
		rd := bufio.NewReader(bytes.NewReader(out))
		for _, p := range paths {
			header, err := rd.ReadString('\n')
			if err != nil {
				return fail(err)
			}
			f := strings.Fields(header)
			if len(f) != 3 {
				return fail(fmt.Errorf("cat-file: unexpected header %q", header))
			}
			n, err := strconv.Atoi(f[2])
			if err != nil {
				return fail(err)
			}
			b := make([]byte, n+1) // content and its trailing newline
			if _, err := io.ReadFull(rd, b); err != nil {
				return fail(err)
			}
			dst := filepath.Join(tmp, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fail(err)
			}
			if err := os.WriteFile(dst, b[:n], 0o644); err != nil {
				return fail(err)
			}
		}
	}
	// The scratch repository sits outside the real one, so the real one's
	// own or includeIf core.excludesFile would not apply to it.
	if out, err := w.Run(ctx, "config", "--path", "core.excludesFile"); err == nil {
		x := strings.TrimSpace(string(out))
		if !filepath.IsAbs(x) {
			x = filepath.Join(w.Root, x)
		}
		if _, err := rules.run(ctx, nil, "config", "core.excludesFile", x); err != nil {
			return fail(err)
		}
	}
	if p, err := w.Run(ctx, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude"); err == nil {
		if b, err := os.ReadFile(strings.TrimSpace(string(p))); err == nil {
			if err := os.WriteFile(filepath.Join(tmp, ".git", "info", "exclude"), b, 0o644); err != nil {
				return fail(err)
			}
		}
	}
	return rules, cleanup, nil
}

// checkIgnore returns which of paths r's ignore rules ignore. The paths
// need not exist; a trailing "/" marks a directory.
func checkIgnore(ctx context.Context, r *Repo, paths []string) (map[string]bool, error) {
	set := map[string]bool{}
	if len(paths) == 0 {
		return set, nil
	}
	out, err := r.RunInput(ctx, []byte(strings.Join(paths, "\x00")+"\x00"), "check-ignore", "--no-index", "-z", "--stdin")
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: none ignored
		return nil, err
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set, nil
}

// expectedIgnored reports whether p, relative to root, is a directory of
// dependencies or build output, which projects routinely ignore. Caches
// and node_modules count anywhere. Build-output and virtualenv names count
// only at the repository root or beside a project manifest, so a package
// that happens to be called tmp or out is still checked. vendor/ counts
// only beside composer.json: Go compiles vendored code.
func expectedIgnored(root, p string) bool {
	if !strings.HasSuffix(p, "/") {
		return false
	}
	parent := path.Dir(strings.TrimSuffix(p, "/"))
	beside := func(names ...string) bool {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(parent), n)); err == nil {
				return true
			}
		}
		return false
	}
	switch path.Base(p) {
	case "node_modules", "__pycache__", ".pytest_cache", ".mypy_cache":
		return true
	case "vendor":
		return beside("composer.json")
	case "dist", "build", "out", "target", "coverage", ".next", ".nuxt", ".turbo", ".cache", "env", "venv", ".venv", "tmp", ".tmp":
		return parent == "." || beside(manifests...)
	}
	return false
}

var manifests = []string{"package.json", "go.mod", "Cargo.toml", "pyproject.toml", "setup.py", "pom.xml", "build.gradle", "build.gradle.kts", "composer.json", "Gemfile", "mix.exs"}

var codeExt = map[string]bool{".go": true, ".ts": true, ".tsx": true, ".mts": true, ".cts": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".vue": true, ".svelte": true, ".py": true, ".rb": true, ".java": true, ".kt": true, ".scala": true, ".rs": true, ".php": true, ".cs": true,
	".swift": true, ".ex": true, ".exs": true, ".lua": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".sql": true, ".sh": true,
	".proto": true, ".tf": true, ".yml": true, ".yaml": true}

// isCode reports whether the file at abs is source worth assessing: a
// known extension, not a minified bundle or source map, at most MaxContent.
func isCode(abs string) bool {
	b := strings.ToLower(filepath.Base(abs))
	if strings.HasSuffix(b, ".min.js") || strings.HasSuffix(b, ".map") {
		return false
	}
	if !codeExt[filepath.Ext(b)] && b != "dockerfile" && b != "makefile" {
		return false
	}
	info, err := os.Lstat(abs)
	return err == nil && info.Mode().IsRegular() && info.Size() <= MaxContent
}

// walkFiles lists regular files under dir, relative to root, skipping
// nested .git and expected-ignored directories. It spends *budget visits
// and reports whether it saw the whole directory.
func walkFiles(root, dir string, budget *int) ([]string, bool) {
	var out []string
	complete := true
	filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(abs string, d os.DirEntry, err error) error {
		if *budget <= 0 {
			complete = false
			return filepath.SkipAll
		}
		*budget--
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || rel+"/" != dir && expectedIgnored(root, rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			out = append(out, rel)
		}
		return nil
	})
	return out, complete
}

func (r *Repo) tempIndex(ctx context.Context) (string, func(), error) {
	out, err := r.Run(ctx, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", nil, err
	}
	src := strings.TrimSpace(string(out))
	if !filepath.IsAbs(src) {
		src = filepath.Join(r.Root, src)
	}
	f, err := os.CreateTemp("", "brisk-index-*")
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	cleanup := func() { os.Remove(f.Name()) }
	if in, err := os.Open(src); err == nil {
		_, err = io.Copy(f, in)
		in.Close()
		if err != nil {
			cleanup()
			return "", nil, err
		}
	} else if !os.IsNotExist(err) {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// loadContents fills Old and New for text files no larger than MaxContent,
// using one `git cat-file --batch` for blobs.
func (r *Repo) loadContents(ctx context.Context, base, head string, files []risk.FileChange) error {
	var specs []string
	type slot struct {
		file int
		old  bool
	}
	var slots []slot
	for i := range files {
		f := &files[i]
		if f.Binary {
			continue
		}
		if f.Status != risk.Added {
			p := f.Path
			if f.OldPath != "" {
				p = f.OldPath
			}
			specs = append(specs, base+":"+p)
			slots = append(slots, slot{i, true})
		}
		if f.Status != risk.Deleted {
			if head == "" {
				b, err := readLimited(filepath.Join(r.Root, f.Path))
				if err == nil {
					f.New = b
				}
				continue
			}
			specs = append(specs, head+":"+f.Path)
			slots = append(slots, slot{i, false})
		}
	}
	if len(specs) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = r.Root
	cmd.Stdin = strings.NewReader(strings.Join(specs, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git cat-file: %w", err)
	}
	br := bufio.NewReader(bytes.NewReader(out))
	for _, s := range slots {
		header, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("git cat-file: short output")
		}
		if strings.HasSuffix(header, " missing\n") {
			continue
		}
		fields := strings.Fields(header)
		if len(fields) < 3 {
			continue
		}
		size, _ := strconv.Atoi(fields[2])
		buf := make([]byte, size+1) // content plus trailing newline
		if _, err := io.ReadFull(br, buf); err != nil {
			return fmt.Errorf("git cat-file: %w", err)
		}
		if size > MaxContent {
			continue
		}
		if s.old {
			files[s.file].Old = buf[:size]
		} else {
			files[s.file].New = buf[:size]
		}
	}
	return nil
}

func readLimited(p string) ([]byte, error) {
	st, err := os.Lstat(p) // never follow a symlink out of the repository
	if err != nil || st.Size() > MaxContent || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("skip")
	}
	return os.ReadFile(p)
}

// ParseDiff parses `git diff` output with a/ and b/ prefixes.
func ParseDiff(raw []byte) ([]risk.FileChange, error) {
	var files []risk.FileChange
	var cur *risk.FileChange
	var hunk *risk.Hunk
	flush := func() {
		if cur != nil {
			if hunk != nil {
				cur.Hunks = append(cur.Hunks, *hunk)
				hunk = nil
			}
			files = append(files, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			a, b := splitDiffHeader(strings.TrimPrefix(line, "diff --git "))
			cur = &risk.FileChange{Path: b, Status: risk.Modified}
			if a != b {
				cur.OldPath = a
			}
			continue
		case cur == nil:
			continue
		}
		if hunk == nil || strings.HasPrefix(line, "@@") {
			switch {
			case strings.HasPrefix(line, "new file mode"):
				cur.Status = risk.Added
				cur.OldPath = ""
			case strings.HasPrefix(line, "deleted file mode"):
				cur.Status = risk.Deleted
			case strings.HasPrefix(line, "rename from "):
				cur.Status = risk.Renamed
				cur.OldPath = unquotePath(strings.TrimPrefix(line, "rename from "))
			case strings.HasPrefix(line, "rename to "):
				cur.Path = unquotePath(strings.TrimPrefix(line, "rename to "))
			case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
				cur.Binary = true
			case strings.HasPrefix(line, "@@"):
				if hunk != nil {
					cur.Hunks = append(cur.Hunks, *hunk)
				}
				h, err := parseHunkHeader(line)
				if err != nil {
					return nil, err
				}
				hunk = &h
			}
			continue
		}
		if line == "" {
			line = " "
		}
		switch line[0] {
		case '+':
			cur.Additions++
		case '-':
			cur.Deletions++
		case '\\':
			continue
		}
		hunk.Lines = append(hunk.Lines, line)
	}
	flush()
	for i := range files {
		if files[i].Status == risk.Modified && files[i].OldPath != "" {
			files[i].Status = risk.Renamed
		}
	}
	return files, sc.Err()
}

func splitDiffHeader(s string) (string, string) {
	// "a/x b/x" with no spaces in paths is the common case; with spaces, the
	// two halves are equal unless renamed, and renames are fixed up by the
	// "rename from/to" lines.
	if strings.HasPrefix(s, `"`) {
		if a, rest, ok := unquoteFirst(s); ok {
			b := strings.TrimSpace(rest)
			if strings.HasPrefix(b, `"`) {
				b, _, _ = unquoteFirst(b)
			}
			return strings.TrimPrefix(a, "a/"), strings.TrimPrefix(b, "b/")
		}
	}
	half := (len(s) - 1) / 2
	if len(s)%2 == 1 && s[half] == ' ' && s[2:half] == s[half+3:] {
		return s[2:half], s[half+3:]
	}
	if i := strings.Index(s, " b/"); i > 0 {
		return strings.TrimPrefix(s[:i], "a/"), s[i+3:]
	}
	return s, s
}

func unquotePath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

func unquoteFirst(s string) (string, string, bool) {
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			u, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", false
			}
			return u, s[i+1:], true
		}
	}
	return "", "", false
}

func parseHunkHeader(line string) (risk.Hunk, error) {
	// @@ -a,b +c,d @@ context
	var h risk.Hunk
	end := strings.Index(line[2:], "@@")
	if end < 0 {
		return h, fmt.Errorf("bad hunk header %q", line)
	}
	for _, f := range strings.Fields(line[2 : end+2]) {
		start, count := f[1:], "1"
		if i := strings.IndexByte(start, ','); i >= 0 {
			start, count = start[:i], start[i+1:]
		}
		s, err1 := strconv.Atoi(start)
		c, err2 := strconv.Atoi(count)
		if err1 != nil || err2 != nil {
			return h, fmt.Errorf("bad hunk header %q", line)
		}
		if f[0] == '-' {
			h.OldStart, h.OldLines = s, c
		} else {
			h.NewStart, h.NewLines = s, c
		}
	}
	return h, nil
}

// AddedLines returns the added lines of a file with their new line numbers.
func AddedLines(fc risk.FileChange) []Line {
	var out []Line
	for _, h := range fc.Hunks {
		n := h.NewStart
		for _, l := range h.Lines {
			switch l[0] {
			case '+':
				out = append(out, Line{N: n, Text: l[1:]})
				n++
			case ' ':
				n++
			}
		}
	}
	return out
}

// RemovedLines returns the removed lines of a file with their old line numbers.
func RemovedLines(fc risk.FileChange) []Line {
	var out []Line
	for _, h := range fc.Hunks {
		n := h.OldStart
		for _, l := range h.Lines {
			switch l[0] {
			case '-':
				out = append(out, Line{N: n, Text: l[1:]})
				n++
			case ' ':
				n++
			}
		}
	}
	return out
}

// Line is one line of a file.
type Line struct {
	N    int
	Text string
}
