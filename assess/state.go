package assess

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ronny/brisk/risk"
	"github.com/ronny/brisk/rules"
)

// MaxStateChars keeps the serialised state under amp.ai.decide's 50,000
// character limit, with room for the questions.
const MaxStateChars = 42000

type stateFile struct {
	Path    string   `json:"path"`
	Status  string   `json:"status"`
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
	Tags    []string `json:"tags,omitempty"`
	History string   `json:"history,omitempty"`
}

type state struct {
	Task         string      `json:"task,omitempty"`
	Summary      string      `json:"summary"`
	Files        []stateFile `json:"files"`
	OmittedFiles int         `json:"omitted_files,omitempty"`
	Structural   []string    `json:"structural_findings,omitempty"`
	Verification []string    `json:"verification,omitempty"`
	Diff         string      `json:"diff"`
}

// buildState turns the change into the text every question is asked
// about. It is deterministic so the same change gives the same request ID.
func buildState(cs *risk.ChangeSet, findings []risk.Finding, hits []rules.Hit, pc risk.ProjectContext, session *risk.Session, task string) any {
	st := state{}
	if session != nil && session.Intent != "" {
		task = session.Intent
	}
	st.Task = clip(rules.RedactSecrets(task), 1500)

	tags := map[string][]string{}
	for _, h := range hits {
		for _, ev := range h.Evidence {
			if ev.Path != "" && !contains(tags[ev.Path], h.ID) {
				tags[ev.Path] = append(tags[ev.Path], h.ID)
			}
		}
	}

	var add, del, tests, code int
	files := append([]risk.FileChange(nil), cs.Files...)
	sort.SliceStable(files, func(i, j int) bool { return priority(files[i], tags) > priority(files[j], tags) })
	for i, f := range files {
		add += f.Additions
		del += f.Deletions
		switch {
		case rules.IsTest(f.Path):
			tests++
		case !rules.IsDoc(f.Path) && !rules.IsGenerated(f.Path):
			code++
		}
		if i >= 60 {
			st.OmittedFiles++
			continue
		}
		sf := stateFile{Path: f.Path, Status: string(f.Status), Added: f.Additions, Removed: f.Deletions, Tags: tags[f.Path]}
		if f.OldPath != "" {
			sf.Path = f.OldPath + " → " + f.Path
		}
		if h, ok := pc.Files[f.Path]; ok && (h.FixCommits > 0 || h.RevertCommits > 0 || h.Commits90d > 0) {
			sf.History = fmt.Sprintf("%d commits in 90 days, %d earlier fixes, %d reverts", h.Commits90d, h.FixCommits, h.RevertCommits)
		}
		st.Files = append(st.Files, sf)
	}
	st.Summary = fmt.Sprintf("%d files changed (%d code, %d test), +%d -%d lines", len(cs.Files), code, tests, add, del)

	for _, f := range findings {
		if f.Kind == risk.ParseError || len(st.Structural) >= 25 {
			continue
		}
		loc := f.Path
		if f.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.StartLine)
		}
		st.Structural = append(st.Structural, fmt.Sprintf("%s: %s (%s)", loc, clip(rules.RedactSecrets(f.Detail), 200), f.Kind))
	}

	st.Verification = verification(cs, session)

	// The diff gets whatever budget is left, highest-priority files first.
	fixed, _ := json.Marshal(st)
	budget := MaxStateChars - len(fixed) - 200
	var b strings.Builder
	for _, f := range files {
		if f.Binary || rules.IsGenerated(f.Path) {
			continue
		}
		chunk := fileDiff(f)
		if b.Len()+len(chunk) > budget {
			left := budget - b.Len()
			if left > 400 {
				b.WriteString(chunk[:left-40])
				b.WriteString("\n[… diff truncated …]\n")
			}
			break
		}
		b.WriteString(chunk)
	}
	st.Diff = b.String()
	return st
}

// verification summarises what the change and session show about testing.
func verification(cs *risk.ChangeSet, session *risk.Session) []string {
	var out []string
	var testFiles int
	for _, f := range cs.Files {
		if rules.IsTest(f.Path) {
			testFiles++
		}
	}
	if testFiles == 0 {
		out = append(out, "The change adds or edits no test files.")
	} else {
		out = append(out, fmt.Sprintf("The change edits %d test files.", testFiles))
	}
	if session == nil {
		return out
	}
	var ran int
	for _, c := range session.Commands {
		if !rules.IsVerification(c.Command) {
			continue
		}
		ran++
		res := "exit code unknown"
		if c.ExitCode != nil {
			res = fmt.Sprintf("exit %d", *c.ExitCode)
		}
		if len(out) < 12 {
			out = append(out, fmt.Sprintf("Ran `%s`: %s", clip(rules.RedactSecrets(c.Command), 120), res))
		}
	}
	if ran == 0 {
		out = append(out, "The agent ran no tests, type checks, or linters in this session.")
	}
	return out
}

func priority(f risk.FileChange, tags map[string][]string) int {
	p := 0
	if len(tags[f.Path]) > 0 {
		p += 100
	}
	switch {
	case rules.IsGenerated(f.Path):
		p -= 50
	case rules.IsDoc(f.Path):
		p -= 20
	case rules.IsTest(f.Path):
		p -= 5
	}
	return p
}

// fileDiff is f's diff for the model, with secrets redacted before any
// clipping so no part of one is sent. Credential files and files holding a
// private key on either side are left out entirely. Files too large to
// load still drop the lines between a key's BEGIN and END lines.
func fileDiff(f risk.FileChange) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s (%s)\n", f.Path, f.Status)
	if rules.IsCredentialFile(f.Path) || privateKey.Match(f.Old) || privateKey.Match(f.New) {
		b.WriteString("[credential file: contents not sent]\n")
		return b.String()
	}
	inKey := false
	for _, h := range f.Hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		for _, l := range h.Lines {
			switch {
			case strings.Contains(l, "PRIVATE KEY-----") && strings.Contains(l, "-----BEGIN"):
				inKey = !strings.Contains(l, "-----END")
				l = rules.RedactSecrets(l)
			case inKey:
				if strings.Contains(l, "-----END") {
					inKey = false
				}
				l = l[:min(1, len(l))] + "[redacted private key]"
			default:
				l = rules.RedactSecrets(l)
			}
			b.WriteString(clip(l, 300))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

var privateKey = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
