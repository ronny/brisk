package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/eval"
	"github.com/ronny/brisk/risk"
)

// injection builds each case in each variant. Without -answers it writes
// the requests to answer; with -answers it reports tiers and floors.
func injection(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("injection", flag.ExitOnError)
	dir := fs.String("dir", "testdata/injection", "case directory")
	work := fs.String("work", "", "scratch directory for the case repos")
	reqOut := fs.String("requests", "", "write requests JSONL here")
	ansPath := fs.String("answers", "", "answers JSONL; report results")
	repeat := fs.Int("repeat", 5, "runs per request")
	fs.Parse(args)
	if *work == "" {
		return fmt.Errorf("-work is required")
	}
	if abs, _ := filepath.Abs(*work); abs == "/" || !strings.Contains(filepath.Base(abs), "inj") {
		return fmt.Errorf("-work %q: use a scratch directory whose name contains \"inj\"; it is deleted first", *work)
	}
	os.RemoveAll(*work)

	var runs map[string][]eval.Run
	var err error
	if *ansPath != "" {
		if runs, err = eval.LoadRuns(*ansPath); err != nil {
			return err
		}
	}
	var lines []eval.HostLine
	type row struct {
		name, variant string
		floor         risk.Tier
		tiers         []risk.Tier
		risks         []float64
		hits          []string
	}
	var rows []row
	fruns, err := eval.PrepareFixtures(ctx, *dir, *work, true, languageBackends())
	if err != nil {
		return err
	}
	cases := map[string]bool{}
	for _, fr := range fruns {
		cases[fr.Case.Name] = true
		pr := fr.Prepared
		{
			for k := 1; k <= *repeat; k++ {
				key := pr.RequestID
				if k > 1 {
					key = fmt.Sprintf("%s#%d", pr.RequestID, k)
				}
				lines = append(lines, eval.HostLine{Key: key, RequestID: pr.RequestID, State: pr.Request.State, Questions: pr.Request.Questions})
			}
			r := row{name: fr.Case.Name, variant: fr.Variant, floor: pr.Floor}
			for _, h := range pr.Hits {
				r.hits = append(r.hits, h.ID)
			}
			for _, run := range runs[pr.RequestID] {
				resp := risk.Response{Model: run.Model, Answers: run.Answers}
				a := pr.Finish("host", &resp, "")
				r.tiers = append(r.tiers, a.Tier)
				if a.Provenance.ModelRisk != nil {
					r.risks = append(r.risks, *a.Provenance.ModelRisk)
				}
			}
			rows = append(rows, r)
		}
	}
	if *reqOut != "" {
		if err := eval.WriteJSONL(*reqOut, lines); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%d cases × %d variants, %d requests\n", len(cases), len(eval.Variants), len(lines))
	}
	if runs == nil {
		return nil
	}
	if t, _, err := calibration.For("jev-1.14"); err == nil {
		fmt.Printf("calibration %s\n", t.ID)
	}
	below := 0
	plainRisk := map[string]float64{}
	fmt.Printf("%-20s %-8s %-6s %-22s %-8s %s\n", "case", "variant", "floor", "tiers", "risk", "rule hits")
	for _, r := range rows {
		mean := 0.0
		for _, x := range r.risks {
			mean += x
		}
		if len(r.risks) > 0 {
			mean /= float64(len(r.risks))
		}
		var ts []string
		for _, t := range r.tiers {
			ts = append(ts, t.String())
			if t < r.floor {
				below++
			}
		}
		if r.variant == "plain" {
			plainRisk[r.name] = mean
		}
		delta := ""
		if r.variant != "plain" {
			delta = fmt.Sprintf(" (%+.3f)", mean-plainRisk[r.name])
		}
		fmt.Printf("%-20s %-8s %-6s %-22s %.3f%s %s\n", r.name, r.variant, r.floor, strings.Join(ts, ","), mean, delta, strings.Join(r.hits, ","))
	}
	fmt.Printf("assessments below their rule floor: %d\n", below)
	return nil
}
