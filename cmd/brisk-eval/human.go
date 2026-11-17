package main

// brisk-eval human measures human_check on the test split, from recorded
// answers only: how often each preset asks for a person, how many bad
// changes it asks about, and whether its questions point at a file a
// later fix changed (prompt hit).
//
//	brisk-eval human -records a,b -answers x -repos name=dir,... -review-requests-out req.jsonl
//	(answer req.jsonl with eval/ampdecide brisk_decide_batch)
//	brisk-eval human ... -review-answers rev.jsonl -report human.json

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/context/gitctx"
	"github.com/ronny/brisk/eval"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
)

// HumanSummary is one preset's human_check numbers on the test split.
type HumanSummary struct {
	N, Bad int
	// Ask is the share of changes human_check asks a person about;
	// AskRecall the share of bad changes it asks about.
	Ask, AskRecall eval.Proportion
	// PromptHit is the share of bad changes it asks about, with a later
	// fix, where a question's evidence is in a file the fix changed.
	PromptHit eval.Proportion
}

func human(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("human", flag.ExitOnError)
	recPaths := fs.String("records", "", "comma-separated records JSONL")
	ansPaths := fs.String("answers", "", "comma-separated answers JSONL")
	repoList := fs.String("repos", "", "comma-separated name=dir for each repository in the records")
	split := fs.Float64("split", 0.6, "training share; only the rest is measured")
	reviewReqOut := fs.String("review-requests-out", "", "write the review-plan requests (answer them like the main ones)")
	reviewAns := fs.String("review-answers", "", "answers to the review-plan requests")
	reportPath := fs.String("report", "", "report JSON")
	presets := fs.String("presets", "light,balanced,careful", "presets to measure")
	fs.Parse(args)

	var recs []eval.Record
	for _, p := range splitList(*recPaths) {
		rs, err := eval.ReadJSONL[eval.Record](p)
		if err != nil {
			return err
		}
		recs = append(recs, rs...)
	}
	_, test := eval.Split(recs, *split)
	runs, err := eval.LoadRuns(splitList(*ansPaths)...)
	if err != nil {
		return err
	}
	reviewRuns, err := eval.LoadRuns(splitList(*reviewAns)...)
	if err != nil {
		return err
	}
	engines := map[string]*assess.Engine{}
	repos := map[string]*gitx.Repo{}
	for _, kv := range splitList(*repoList) {
		name, dir, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("-repos: want name=dir, got %q", kv)
		}
		repo, err := gitx.Open(ctx, dir)
		if err != nil {
			return err
		}
		prov := &gitctx.Provider{}
		head, err := repo.RevParse(ctx, "HEAD")
		if err != nil {
			return err
		}
		if _, err := prov.Context(ctx, risk.ContextQuery{RepoRoot: repo.Root, Base: head}); err != nil {
			return err
		}
		repos[name], engines[name] = repo, &assess.Engine{Repo: repo, Lang: languageBackends(), Context: prov}
	}

	var names []string
	for _, n := range splitList(*presets) {
		if a, ok := assess.PresetAliases[n]; ok {
			n = a
		}
		if _, ok := assess.Presets[n]; !ok {
			return fmt.Errorf("unknown preset %q", n)
		}
		names = append(names, n)
	}

	type row struct {
		rec      eval.Record
		needed   map[string]bool
		consider map[string][]risk.Consider
		fix      map[string]bool
	}
	var rows []row
	var reviewReqs []eval.HostLine
	mismatched := 0
	for _, r := range test {
		eng := engines[r.Repo]
		if eng == nil {
			return fmt.Errorf("no -repos entry for %s", r.Repo)
		}
		runsFor := runs[r.RequestID]
		if len(runsFor) == 0 {
			continue
		}
		p, err := eng.Prepare(ctx, assess.Input{Base: r.Parent, Head: r.SHA, Task: r.Message})
		if err != nil {
			return fmt.Errorf("%s %s: %w", r.Repo, r.SHA[:7], err)
		}
		if p.RequestID != r.RequestID {
			mismatched++
			continue
		}
		if rq := p.ReviewRequest(); rq != nil && *reviewReqOut != "" {
			reviewReqs = append(reviewReqs, eval.HostLine{Key: r.RequestID + "#review", RequestID: r.RequestID, State: rq.State, Questions: rq.Questions})
		}
		if rr := reviewRuns[r.RequestID]; len(rr) > 0 {
			p.ReviewAnswers = rr[0].Answers
		}
		resp := &risk.Response{Model: runsFor[0].Model, Answers: runsFor[0].Answers}
		x := row{rec: r, needed: map[string]bool{}, consider: map[string][]risk.Consider{}}
		for _, n := range names {
			// The built-in preset only: the repositories' own .brisk.yaml
			// questions and review_checks areas are not measured.
			p.Policy = assess.Presets[n]
			a := p.Finish("host", resp, "")
			x.needed[n], x.consider[n] = a.HumanCheck.Needed, a.HumanCheck.Consider
		}
		if len(r.FixedBy) > 0 {
			if x.fix, err = eval.FixFiles(ctx, repos[r.Repo], r.FixedBy); err != nil {
				return err
			}
		}
		rows = append(rows, x)
	}
	fmt.Fprintf(os.Stderr, "human: %d test changes scored, %d skipped because the request changed\n", len(rows), mismatched)
	if *reviewReqOut != "" {
		if err := eval.WriteJSONL(*reviewReqOut, reviewReqs); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "human: %d review-plan requests in %s\n", len(reviewReqs), *reviewReqOut)
	}

	out := map[string]HumanSummary{}
	for _, n := range names {
		var s HumanSummary
		var ask, askBad, withFix, hit int
		for _, x := range rows {
			s.N++
			if x.needed[n] {
				ask++
			}
			if !x.rec.Bad {
				continue
			}
			s.Bad++
			if x.needed[n] {
				askBad++
				if len(x.fix) > 0 {
					withFix++
					if eval.PromptHit(x.consider[n], x.fix) {
						hit++
					}
				}
			}
		}
		s.Ask, s.AskRecall = eval.NewProportion(ask, s.N), eval.NewProportion(askBad, s.Bad)
		s.PromptHit = eval.NewProportion(hit, withFix)
		out[n] = s
		fmt.Printf("preset %-9s asks %.1f%% (%d/%d) | asks about %.0f%% of bad | prompt hit %.0f%% (%d/%d)\n",
			n, 100*s.Ask.Value, ask, s.N, 100*s.AskRecall.Value, 100*s.PromptHit.Value, hit, withFix)
	}
	if *reportPath != "" {
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(*reportPath, append(b, '\n'), 0o644)
	}
	return nil
}
