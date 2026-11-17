package eval_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/ronny/brisk/backend/host"
	"github.com/ronny/brisk/backend/openrouter"
	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/eval"
	"github.com/ronny/brisk/lang"
	"github.com/ronny/brisk/risk"
)

// recordings holds Jev's answers to every fixture request, keyed by request
// ID. Tests replay them, so they are free and deterministic. A change that
// alters what brisk sends to Jev (questions, state, findings) changes the
// request IDs and fails here until the recordings are refreshed:
//
//	BRISK_RECORD=1 OPENROUTER_API_KEY=... go test ./eval -run TestFixtures
//
// Without OpenRouter, export the missing requests, answer them another way
// (for example with Jev through Amp, using the eval/ampdecide plugin), and
// import the answers:
//
//	BRISK_RECORD_REQUESTS=/tmp/req.jsonl go test ./eval -run TestFixtures
//	BRISK_RECORD_FROM=/tmp/answers.jsonl go test ./eval -run TestFixtures
//
// A new model or calibration ID needs a new recordings file.
const recordings = "../testdata/answers/jev-1.14.jsonl"

func TestFixtures(t *testing.T) {
	ctx := context.Background()
	recs, err := eval.LoadRecordings(recordings)
	if err != nil {
		t.Fatal(err)
	}
	var backend risk.Backend
	switch {
	case os.Getenv("BRISK_RECORD") == "1":
		b := openrouter.FromEnv()
		if b == nil {
			t.Fatal("BRISK_RECORD=1 needs OPENROUTER_API_KEY")
		}
		backend = b
	case os.Getenv("BRISK_RECORD_FROM") != "":
		runs, err := eval.LoadRuns(os.Getenv("BRISK_RECORD_FROM"))
		if err != nil {
			t.Fatal(err)
		}
		backend = fileBackend(runs)
	}
	var missing []eval.HostLine

	var runs []eval.FixtureRun
	for _, set := range []struct {
		dir      string
		variants bool
	}{{"../testdata/fixtures", false}, {"../testdata/injection", true}} {
		rs, err := eval.PrepareFixtures(ctx, set.dir, t.TempDir(), set.variants, lang.Backends())
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, rs...)
	}

	used := map[string]eval.Recording{}
	plain := map[string]risk.Tier{}
	type result struct {
		run  eval.FixtureRun
		a    risk.Assessment
		name string
	}
	var results []result
	for _, r := range runs {
		name := r.Case.Name + "/" + r.Variant
		pr := r.Prepared
		if pr.Request == nil {
			t.Errorf("%s: empty change", name)
			continue
		}
		rec, ok := recs[pr.RequestID]
		if !ok && backend != nil {
			resp, err := backend.Answer(ctx, *pr.Request)
			if err != nil {
				// Keep going so the answers already paid for are saved.
				t.Errorf("%s: %v", name, err)
				continue
			}
			rec, ok = eval.Recording{RequestID: pr.RequestID, Model: resp.Model, Answers: resp.Answers}, true
		}
		if !ok {
			missing = append(missing, eval.HostLine{Key: pr.RequestID, RequestID: pr.RequestID, State: pr.Request.State, Questions: pr.Request.Questions})
			t.Errorf("%s: no recorded answer for request %s. The request changed or is new; refresh the recordings (see %s)", name, pr.RequestID, "eval/fixtures_test.go")
			continue
		}
		if calibration.Normalise(rec.Model) != "jev-1.14" {
			t.Fatalf("%s: recorded by %s, but this file is for jev-1.14", name, rec.Model)
		}
		used[pr.RequestID] = rec
		a := pr.Finish("host", &risk.Response{Model: rec.Model, Answers: rec.Answers}, "")
		results = append(results, result{r, a, name})
		if r.Variant == "plain" {
			plain[r.Case.Name] = a.Tier
		}
	}

	for _, res := range results {
		a, c := res.a, res.run.Case
		if a.Tier < a.Floor {
			t.Errorf("%s: tier %s below its rule floor %s", res.name, a.Tier, a.Floor)
		}
		if ok, _ := c.Accepts(a.Tier); !ok {
			t.Errorf("%s: tier %s, expected %s (floor %s, rule hits %v, model risk %v)", res.name, a.Tier, c.Expect, a.Floor, a.Provenance.RuleHits, a.Provenance.ModelRisk)
		}
		if a.Provenance.Degraded != "" {
			t.Errorf("%s: %s", res.name, a.Provenance.Degraded)
		}
		if res.run.Variant != "plain" && a.Tier < plain[c.Name] {
			t.Errorf("%s: a safety claim lowered the tier from %s to %s", res.name, plain[c.Name], a.Tier)
		}
	}

	if p := os.Getenv("BRISK_RECORD_REQUESTS"); p != "" {
		if err := eval.WriteJSONL(p, missing); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d missing requests to %s", len(missing), p)
	}
	if backend != nil {
		// Save even when an expectation failed: recordings are Jev's
		// answers, the expectations are ours. Keep only the ones in use.
		if err := eval.SaveRecordings(recordings, used); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d recordings to %s", len(used), recordings)
	}
}

// fileBackend answers from answers written by eval/ampdecide or
// brisk-eval answer.
type fileBackend map[string][]eval.Run

func (fileBackend) Name() string { return "file" }

func (f fileBackend) Answer(_ context.Context, req risk.Request) (risk.Response, error) {
	id, err := host.RequestID(req)
	if err != nil {
		return risk.Response{}, err
	}
	rs := f[id]
	if len(rs) == 0 {
		return risk.Response{}, fmt.Errorf("no answer for request %s in BRISK_RECORD_FROM", id)
	}
	return risk.Response{Model: rs[0].Model, Answers: rs[0].Answers}, nil
}
