// Package eval measures brisk offline on git history: outcome labels from
// reverts, SZZ-style fixes, and CI failures; baselines; and the metrics in
// the MVP plan's definition of done.
package eval

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ronny/brisk/context/gitctx"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
	"github.com/ronny/brisk/rules"
)

// FixWindow is how soon after a change a fix must land to count against it.
const FixWindow = 14 * 24 * time.Hour

// Commit is one commit on the first-parent history.
type Commit struct {
	SHA     string    `json:"sha"`
	Parent  string    `json:"parent"`
	When    time.Time `json:"when"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
}

// Message is the full commit message.
func (c Commit) Message() string { return strings.TrimSpace(c.Subject + "\n\n" + c.Body) }

// Label is the outcome of one commit.
type Label struct {
	Repo     string    `json:"repo"`
	SHA      string    `json:"sha"`
	Parent   string    `json:"parent"`
	When     time.Time `json:"when"`
	Subject  string    `json:"subject"`
	Message  string    `json:"message"`
	AmpMade  bool      `json:"amp_made"`
	Reverted bool      `json:"reverted"`
	// FixedBy lists fix commits within FixWindow that change lines this
	// commit introduced.
	FixedBy  []string `json:"fixed_by,omitempty"`
	CIFailed bool     `json:"ci_failed"`
	Bad      bool     `json:"bad"`
	// Censored commits are too recent for a fix to have landed; they are
	// left out of the metrics.
	Censored bool `json:"censored"`
	// IsFix says whether this commit is itself a fix (keyword or model).
	IsFix bool `json:"is_fix"`
}

// Commits lists first-parent, non-merge commits from since to HEAD,
// oldest first, skipping root commits.
func Commits(ctx context.Context, repo *gitx.Repo, since time.Time) ([]Commit, error) {
	out, err := repo.Run(ctx, "log", "--first-parent", "--no-merges", "--since="+since.Format(time.RFC3339),
		"--format=%x1e%H%x1f%P%x1f%ct%x1f%s%x1f%b", "HEAD")
	if err != nil {
		return nil, err
	}
	var cs []Commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		f := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x1f", 5)
		if len(f) < 5 {
			continue
		}
		parents := strings.Fields(f[1])
		if len(parents) == 0 {
			continue
		}
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		cs = append(cs, Commit{SHA: f[0], Parent: parents[0], When: time.Unix(ts, 0).UTC(), Subject: f[3], Body: strings.TrimSpace(f[4])})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].When.Before(cs[j].When) })
	return cs, nil
}

var revertOf = regexp.MustCompile(`(?i)this reverts commit ([0-9a-f]{7,40})`)
var revertSubject = regexp.MustCompile(`^Revert "(.+)"$`)

// FixQuestion asks whether a commit message describes a bug fix. It labels
// the later commit, never the change being assessed.
var FixQuestion = map[string]risk.Question{
	"is_fix": {
		Type:         "noul",
		Instructions: "Does this commit fix a bug, regression, or wrong behaviour in existing code, as opposed to adding a feature, refactoring, upgrading dependencies, or editing documentation?",
	},
}

// LabelOptions configures Labels.
type LabelOptions struct {
	Repo     string
	Since    time.Time
	CIFailed map[string]bool
	// IsFix returns the model's answer for a commit message, or false when
	// it has none.
	IsFix func(sha string) (float64, bool)
	// FixThreshold is the model probability above which a commit is a fix.
	FixThreshold float64
}

// Labels computes outcome labels for every commit since opts.Since.
func Labels(ctx context.Context, repo *gitx.Repo, opts LabelOptions) ([]Label, error) {
	all, err := Commits(ctx, repo, opts.Since.Add(-FixWindow))
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no commits since %s", opts.Since)
	}
	head := all[len(all)-1].When
	byShort := map[string]string{}
	bySubject := map[string]string{}
	index := map[string]int{}
	for i, c := range all {
		byShort[c.SHA[:7]] = c.SHA
		bySubject[c.Subject] = c.SHA
		index[c.SHA] = i
	}

	labels := map[string]*Label{}
	for _, c := range all {
		if c.When.Before(opts.Since) {
			continue
		}
		labels[c.SHA] = &Label{Repo: opts.Repo, SHA: c.SHA, Parent: c.Parent, When: c.When, Subject: c.Subject, Message: c.Message(),
			AmpMade:  strings.Contains(c.Body, "Amp-Thread-ID") || strings.Contains(c.Body, "Co-authored-by: Amp"),
			CIFailed: opts.CIFailed[c.SHA], Censored: head.Sub(c.When) < FixWindow}
	}

	isFix := func(c Commit) bool {
		if gitctx.FixMessage.MatchString(c.Subject) {
			return true
		}
		if opts.IsFix != nil {
			if p, ok := opts.IsFix(c.SHA); ok && p >= opts.FixThreshold {
				return true
			}
		}
		return false
	}

	for _, c := range all {
		// Reverts.
		reverted := ""
		if m := revertOf.FindStringSubmatch(c.Body); m != nil {
			for short, full := range byShort {
				if strings.HasPrefix(m[1], short) || strings.HasPrefix(full, m[1]) {
					reverted = full
					break
				}
			}
		} else if m := revertSubject.FindStringSubmatch(c.Subject); m != nil {
			reverted = bySubject[m[1]]
		}
		if l := labels[reverted]; l != nil {
			l.Reverted = true
		}
		fix := reverted != "" || isFix(c)
		if l := labels[c.SHA]; l != nil {
			l.IsFix = fix
		}
		if !fix || reverted != "" {
			continue
		}
		// SZZ: blame the lines the fix removes or changes.
		blamed, err := szz(ctx, repo, c)
		if err != nil {
			return nil, fmt.Errorf("szz %s: %w", c.SHA[:7], err)
		}
		for sha := range blamed {
			l := labels[sha]
			if l == nil || sha == c.SHA {
				continue
			}
			if d := c.When.Sub(l.When); d >= 0 && d <= FixWindow {
				l.FixedBy = append(l.FixedBy, c.SHA)
			}
		}
	}

	var out []Label
	for _, l := range labels {
		l.Bad = l.Reverted || len(l.FixedBy) > 0 || l.CIFailed
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].When.Before(out[j].When) })
	return out, nil
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// szz returns the commits that last touched the lines fix removes or
// changes, ignoring docs, tests, generated files, and whitespace.
func szz(ctx context.Context, repo *gitx.Repo, fix Commit) (map[string]bool, error) {
	out, err := repo.Run(ctx, "diff", "-U0", "-w", "--no-renames", "--no-ext-diff", fix.Parent, fix.SHA)
	if err != nil {
		return nil, err
	}
	ranges := map[string][][2]int{}
	file := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "--- "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
			if file == "/dev/null" || rules.IsDoc(file) || rules.IsTest(file) || rules.IsGenerated(file) {
				file = ""
			}
		case file != "" && strings.HasPrefix(line, "@@"):
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if n > 0 {
				ranges[file] = append(ranges[file], [2]int{start, start + n - 1})
			}
		}
	}
	blamed := map[string]bool{}
	for f, rs := range ranges {
		args := []string{"blame", "--porcelain", "-w"}
		for _, r := range rs {
			args = append(args, "-L", fmt.Sprintf("%d,%d", r[0], r[1]))
		}
		args = append(args, fix.Parent, "--", f)
		out, err := repo.Run(ctx, args...)
		if err != nil {
			continue // file too new or binary
		}
		for _, line := range strings.Split(string(out), "\n") {
			if len(line) > 41 && line[40] == ' ' && isHex(line[:40]) {
				blamed[line[:40]] = true
			}
		}
	}
	return blamed, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
