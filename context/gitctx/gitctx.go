// Package gitctx is the MVP context provider: project rules from
// .brisk.yaml, owners from CODEOWNERS, and per-file history from git.
package gitctx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"

	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
)

// Provider reads context from the repository at the query's RepoRoot.
type Provider struct {
	// MaxCommits caps how much history is read. Default 5000.
	MaxCommits int
	// ConfigPath overrides <repo>/.brisk.yaml.
	ConfigPath string

	mu      sync.Mutex
	history *historyCache
}

var _ risk.ContextProvider = (*Provider)(nil)

// Config is the .brisk.yaml file: project path rules plus the team's risk
// policy (preset, thresholds, actions, agent review).
type Config struct {
	SensitivePaths []struct {
		ID     string   `yaml:"id"`
		Paths  []string `yaml:"paths"`
		Floor  string   `yaml:"floor"`
		Reason string   `yaml:"reason"`
	} `yaml:"sensitive_paths"`
	risk.PolicyConfig `yaml:",inline"`
}

// LoadConfig reads a .brisk.yaml file. A missing file is not an error.
func LoadConfig(p string) ([]risk.PathRule, risk.PolicyConfig, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, risk.PolicyConfig{}, nil
	}
	if err != nil {
		return nil, risk.PolicyConfig{}, err
	}
	return ParseConfig(p, b)
}

// ParseConfig parses .brisk.yaml content; p names it in errors. Unknown
// keys are errors, so a misspelt setting does not silently do nothing.
func ParseConfig(p string, b []byte) ([]risk.PathRule, risk.PolicyConfig, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && err != io.EOF {
		return nil, risk.PolicyConfig{}, fmt.Errorf("%s: %w", p, err)
	}
	var more any
	if err := dec.Decode(&more); err != io.EOF {
		return nil, risk.PolicyConfig{}, fmt.Errorf("%s: only one YAML document is allowed", p)
	}
	var out []risk.PathRule
	for _, r := range c.SensitivePaths {
		t, err := risk.ParseTier(r.Floor)
		if err != nil {
			return nil, risk.PolicyConfig{}, fmt.Errorf("%s: sensitive path %q: %w", p, r.ID, err)
		}
		if r.ID == "" || len(r.Paths) == 0 {
			return nil, risk.PolicyConfig{}, fmt.Errorf("%s: each sensitive path needs an id and paths", p)
		}
		for _, g := range r.Paths {
			if !doublestar.ValidatePattern(g) {
				return nil, risk.PolicyConfig{}, fmt.Errorf("%s: bad glob %q", p, g)
			}
		}
		out = append(out, risk.PathRule{ID: r.ID, Paths: r.Paths, Floor: t, Reason: r.Reason})
	}
	return out, c.PolicyConfig, nil
}

// Context implements risk.ContextProvider.
func (p *Provider) Context(ctx context.Context, q risk.ContextQuery) (risk.ProjectContext, error) {
	var pc risk.ProjectContext
	// The config comes from the base commit, so a change cannot loosen
	// the rules that judge it. --config overrides it explicitly.
	var err error
	if p.ConfigPath != "" {
		pc.SensitivePaths, pc.Policy, err = LoadConfig(p.ConfigPath)
	} else {
		r := &gitx.Repo{Root: q.RepoRoot}
		if b, gerr := r.Run(ctx, "show", q.Base+":.brisk.yaml"); gerr == nil {
			pc.SensitivePaths, pc.Policy, err = ParseConfig(q.Base+":.brisk.yaml", b)
		}
	}
	if err != nil {
		return pc, err
	}

	owners := loadCodeowners(q.RepoRoot)
	hist, err := p.historyFor(ctx, q.RepoRoot, q.Base)
	if err != nil {
		return pc, err
	}
	pc.Files = map[string]risk.FileHistory{}
	for _, path := range q.Paths {
		fh := hist.summarise(path)
		fh.Owners = owners.match(path)
		pc.Files[path] = fh
	}
	return pc, nil
}

// ---------------------------------------------------------------------------
// History

type commit struct {
	id     string
	when   time.Time
	author string
	fix    bool
	revert bool
	files  []string
}

// history is the log from some commit back, newest first.
type history struct {
	commits []commit
	byFile  map[string][]int
	index   map[string]int // commit ID → position
}

type historyCache struct {
	repo string
	h    *history
}

// view is the history as seen from one base commit: positions >= start
// that are ancestors of the base. The log is in date order, so commits from
// branches merged after the base can sit among older positions.
type view struct {
	h         *history
	start     int
	ancestors map[string]bool // nil means every commit counts
}

// FixMessage matches commit subjects that describe a fix.
var FixMessage = regexp.MustCompile(`(?i)\b(fix(e[sd])?|bug|hotfix|regression|broke|broken|crash(es|ed)?|revert)\b`)

// RevertMessage matches revert commit subjects.
var RevertMessage = regexp.MustCompile(`(?i)^revert\b|\bthis reverts commit\b`)

func (p *Provider) historyFor(ctx context.Context, root, base string) (view, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.history != nil && p.history.repo == root {
		if i, ok := p.history.h.index[base]; ok {
			anc, err := ancestors(ctx, root, base)
			if err != nil {
				return view{}, err
			}
			return view{p.history.h, i, anc}, nil
		}
	}
	max := p.MaxCommits
	if max == 0 {
		max = 5000
	}
	r := &gitx.Repo{Root: root}
	out, err := r.Run(ctx, "log", "-n", strconv.Itoa(max), "--no-merges", "--format=%x1e%H%x1f%ct%x1f%ae%x1f%s", "--name-only", base)
	if err != nil {
		return view{}, err
	}
	h := &history{byFile: map[string][]int{}, index: map[string]int{}}
	for _, rec := range bytes.Split(out, []byte{0x1e}) {
		if len(rec) == 0 {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(rec))
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		if !sc.Scan() {
			continue
		}
		parts := strings.SplitN(sc.Text(), "\x1f", 4)
		if len(parts) < 4 {
			continue
		}
		ts, _ := strconv.ParseInt(parts[1], 10, 64)
		c := commit{id: parts[0], when: time.Unix(ts, 0), author: parts[2], fix: FixMessage.MatchString(parts[3]), revert: RevertMessage.MatchString(parts[3])}
		for sc.Scan() {
			if f := strings.TrimSpace(sc.Text()); f != "" {
				c.files = append(c.files, f)
			}
		}
		i := len(h.commits)
		h.index[c.id] = i
		h.commits = append(h.commits, c)
		for _, f := range c.files {
			h.byFile[f] = append(h.byFile[f], i)
		}
	}
	p.history = &historyCache{repo: root, h: h}
	return view{h: h}, nil
}

func ancestors(ctx context.Context, root, base string) (map[string]bool, error) {
	out, err := (&gitx.Repo{Root: root}).Run(ctx, "rev-list", "--no-merges", base)
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		m[l] = true
	}
	return m, nil
}

func (v view) summarise(path string) risk.FileHistory {
	var fh risk.FileHistory
	if v.h == nil || v.start >= len(v.h.commits) {
		return fh
	}
	authors := map[string]bool{}
	since := v.h.commits[v.start].when.Add(-90 * 24 * time.Hour)
	for _, i := range v.h.byFile[path] {
		if i < v.start || v.ancestors != nil && !v.ancestors[v.h.commits[i].id] {
			continue
		}
		c := v.h.commits[i]
		if !c.when.Before(since) {
			fh.Commits90d++
			authors[c.author] = true
		}
		if c.revert {
			fh.RevertCommits++
		} else if c.fix {
			fh.FixCommits++
		}
	}
	fh.Authors90d = len(authors)
	return fh
}

// ---------------------------------------------------------------------------
// CODEOWNERS

type ownerRule struct {
	pattern string
	owners  []string
}

type codeowners []ownerRule

func loadCodeowners(root string) codeowners {
	for _, p := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS", ".gitlab/CODEOWNERS"} {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			continue
		}
		var rules codeowners
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
				continue
			}
			f := strings.Fields(line)
			rules = append(rules, ownerRule{pattern: f[0], owners: f[1:]})
		}
		return rules
	}
	return nil
}

// match returns the owners of the last matching rule, as in GitHub.
func (c codeowners) match(p string) []string {
	for i := len(c) - 1; i >= 0; i-- {
		if codeownersMatch(c[i].pattern, p) {
			return c[i].owners
		}
	}
	return nil
}

func codeownersMatch(pattern, p string) bool {
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !anchored && !strings.Contains(strings.TrimSuffix(pattern, "/**"), "/") {
		pattern = "**/" + pattern
	}
	if ok, _ := doublestar.Match(pattern, p); ok {
		return true
	}
	ok, _ := doublestar.Match(pattern+"/**", p)
	return ok
}
