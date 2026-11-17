// Package gitx runs git and parses its output. brisk needs only the git
// binary; it never changes the repository, its index, or its config.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

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
func (r *Repo) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false", "-c", "color.ui=false"}, args...)...)
	cmd.Dir = r.Root
	cmd.Env = append(append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"), r.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
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
