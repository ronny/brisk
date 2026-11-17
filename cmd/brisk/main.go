// Command brisk rates how risky a code change is.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/backend/host"
	"github.com/ronny/brisk/backend/openrouter"
	"github.com/ronny/brisk/context/gitctx"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/lang"
	"github.com/ronny/brisk/risk"
)

var languageBackends = lang.Backends

const usage = `usage: brisk assess [flags]
       brisk version

Rates how risky the change from --base to --head (or the working tree) is.

Flags:
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

type usageError struct{ error }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, "brisk", assess.Version)
		return 0
	case "assess":
		code, err := assessCmd(ctx, args[1:], stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "brisk:", err)
			var ue usageError
			if errors.As(err, &ue) {
				return 2
			}
			return 1
		}
		return code
	}
	fmt.Fprintf(stderr, "brisk: unknown command %q\n%s", args[0], usage)
	return 2
}

func assessCmd(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("assess", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoDir := fs.String("repo", ".", "repository `dir`")
	base := fs.String("base", "", "base `ref` (default: merge base with origin/HEAD, main, or master)")
	head := fs.String("head", "", "head `ref` (default: the working tree, including untracked files)")
	sessionPath := fs.String("session", "", "session record `file` (schema/session.schema.json)")
	backendName := fs.String("backend", "auto", "auto, openrouter, host, or rules-only")
	answersPath := fs.String("answers", "", "host answers `file` (schema/host-answers.schema.json)")
	format := fs.String("format", "text", "text or json")
	failOn := fs.String("fail-on", "", "exit 3 when the tier is at least this (medium or high)")
	configPath := fs.String("config", "", "project config `file` (default: <repo>/.brisk.yaml)")
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if fs.NArg() > 0 {
		return 0, usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	if *format != "text" && *format != "json" {
		return 0, usageError{fmt.Errorf("--format must be text or json")}
	}
	var failTier *risk.Tier
	if *failOn != "" {
		t, err := risk.ParseTier(*failOn)
		if err != nil {
			return 0, usageError{err}
		}
		failTier = &t
	}

	var backend risk.Backend
	hostRequest := false
	switch {
	case *answersPath != "":
		if *backendName != "auto" && *backendName != "host" {
			return 0, usageError{fmt.Errorf("--answers needs --backend host or auto")}
		}
		a, err := host.Load(*answersPath)
		if err != nil {
			return 0, err
		}
		backend = a
	case *backendName == "host":
		hostRequest = true
	case *backendName == "openrouter":
		b := openrouter.FromEnv()
		if b == nil {
			return 0, usageError{fmt.Errorf("--backend openrouter needs OPENROUTER_API_KEY")}
		}
		backend = b
	case *backendName == "auto":
		if b := openrouter.FromEnv(); b != nil {
			backend = b
		}
	case *backendName == "rules-only":
	default:
		return 0, usageError{fmt.Errorf("unknown --backend %q", *backendName)}
	}

	var session *risk.Session
	if *sessionPath != "" {
		b, err := os.ReadFile(*sessionPath)
		if err != nil {
			return 0, err
		}
		session = &risk.Session{}
		if err := json.Unmarshal(b, session); err != nil {
			return 0, fmt.Errorf("%s: %w", *sessionPath, err)
		}
	}

	repo, err := gitx.Open(ctx, *repoDir)
	if err != nil {
		return 0, err
	}
	if *base == "" {
		*base, err = repo.DefaultBase(ctx)
		if err != nil {
			return 0, usageError{err}
		}
	}
	eng := &assess.Engine{Repo: repo, Lang: languageBackends(), Context: &gitctx.Provider{ConfigPath: *configPath}}
	p, err := eng.Prepare(ctx, assess.Input{Base: *base, Head: *head, Session: session})
	if err != nil {
		return 0, err
	}

	if hostRequest && p.Request != nil {
		id, err := host.RequestID(*p.Request, p.ReviewQuestions, p.Changes)
		if err != nil {
			return 0, err
		}
		req := risk.HostRequest{
			Schema:          risk.HostRequestSchema,
			RequestID:       id,
			Model:           "typesafe/jev",
			State:           p.Request.State,
			Questions:       p.Request.Questions,
			ReviewQuestions: p.ReviewQuestions,
			ReviewWhen:      reviewWhen(p),
			Floor:           p.Finish("rules-only", nil, ""),
		}
		return 0, writeJSON(stdout, req)
	}

	var a risk.Assessment
	if h, ok := backend.(*host.Answers); ok {
		if p.Request == nil {
			return 0, fmt.Errorf("--answers given, but there is nothing to ask: the change is empty")
		}
		h.Change, h.Review = p.Changes, p.ReviewQuestions
		resp, err := backend.Answer(ctx, *p.Request)
		if err != nil {
			return 0, err
		}
		p.ReviewAnswers, p.ReviewError, p.ReviewUsage = h.ReviewResult()
		a = p.Finish(backend.Name(), &resp, "")
	} else {
		a = p.Answer(ctx, backend)
	}

	if *format == "json" {
		if err := writeJSON(stdout, a); err != nil {
			return 0, err
		}
	} else {
		writeText(stdout, a)
	}
	if failTier != nil && a.Tier >= *failTier {
		return 3, nil
	}
	return 0, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func writeText(w io.Writer, a risk.Assessment) {
	fmt.Fprintf(w, "Risk: %s", strings.ToUpper(a.Tier.String()))
	if a.Floor > risk.Low {
		fmt.Fprintf(w, " (rules floor: %s)", a.Floor)
	}
	if a.Provenance.Answers != nil {
		fmt.Fprintf(w, "  impact %.1f/3  uncertainty %.1f/3", a.Impact, a.Uncertainty)
	}
	fmt.Fprintf(w, "\n%d files, +%d -%d\n", a.Stats.Files, a.Stats.Additions, a.Stats.Deletions)
	for _, r := range a.Reasons {
		fmt.Fprintf(w, "  - %s [%s %s]", r.Text, r.Source, r.ID)
		var locs []string
		for _, e := range r.Evidence {
			if e.Path == "" {
				continue
			}
			l := e.Path
			if e.StartLine > 0 {
				l = fmt.Sprintf("%s:%d", e.Path, e.StartLine)
			}
			if e.Symbol != "" {
				l += " " + e.Symbol
			}
			locs = append(locs, l)
		}
		if len(locs) > 0 {
			fmt.Fprintf(w, "\n      %s", strings.Join(locs, ", "))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Action: %s\n", a.Action)
	if a.AgentReview.Recommended {
		fmt.Fprintf(w, "Agent review: recommended. %s\n", a.AgentReview.Reason)
	}
	src := a.Provenance.Backend
	if a.Provenance.Model != "" {
		src += ", " + a.Provenance.Model
	}
	if a.Provenance.Degraded != "" {
		src += ", fell back to rules only: " + a.Provenance.Degraded
	}
	fmt.Fprintf(w, "(%s, %d ms)\n", src, a.Provenance.DurationMS)
}

// reviewWhen tells a harness when to answer the review questions.
func reviewWhen(p *assess.Prepared) string {
	if len(p.ReviewQuestions) == 0 {
		return ""
	}
	return p.Policy.TailorPlan()
}
