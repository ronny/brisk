// Command brisk-eval runs brisk's offline evaluation on git history.
//
//	brisk-eval fixq     -repo DIR -since DATE -out fixq.jsonl
//	brisk-eval label    -repo DIR -name NAME -since DATE [-ci-failed FILE] [-fix-answers FILE] -out labels.jsonl
//	brisk-eval prepare  -repo DIR -labels labels.jsonl -out records.jsonl -requests requests.jsonl [-repeat 5 -repeat-sample 200 -repeat-out FILE]
//	brisk-eval score    -records a.jsonl,b.jsonl -answers x.jsonl,y.jsonl [-split 0.6] [-fit-out calibration/jev-1.14.json] -report report.json
//
// Requests are answered by the eval/ampdecide Amp plugin (Jev through
// amp.ai.decide) or by `brisk-eval answer` with OPENROUTER_API_KEY.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/backend/openrouter"
	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/context/gitctx"
	"github.com/ronny/brisk/eval"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/lang"
	"github.com/ronny/brisk/risk"
)

var languageBackends = lang.Backends

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: brisk-eval fixq|label|prepare|answer|score|human|injection [flags]")
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "fixq":
		err = fixq(ctx, os.Args[2:])
	case "label":
		err = label(ctx, os.Args[2:])
	case "prepare":
		err = prepare(ctx, os.Args[2:])
	case "answer":
		err = answer(ctx, os.Args[2:])
	case "score":
		err = score(os.Args[2:])
	case "human":
		err = human(ctx, os.Args[2:])
	case "injection":
		err = injection(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "brisk-eval:", err)
		os.Exit(1)
	}
}

func parseDate(s string) (time.Time, error) { return time.Parse("2006-01-02", s) }

func fixq(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fixq", flag.ExitOnError)
	repoDir := fs.String("repo", ".", "repository")
	since := fs.String("since", "", "first day (YYYY-MM-DD)")
	out := fs.String("out", "", "output JSONL")
	fs.Parse(args)
	t, err := parseDate(*since)
	if err != nil {
		return err
	}
	repo, err := gitx.Open(ctx, *repoDir)
	if err != nil {
		return err
	}
	cs, err := eval.Commits(ctx, repo, t)
	if err != nil {
		return err
	}
	var lines []eval.HostLine
	for _, c := range cs {
		k := "fix:" + c.SHA
		lines = append(lines, eval.HostLine{Key: k, RequestID: k, State: c.Message(), Questions: eval.FixQuestion})
	}
	fmt.Fprintf(os.Stderr, "%d commits\n", len(lines))
	return eval.WriteJSONL(*out, lines)
}

func readSet(path string) (map[string]bool, error) {
	m := map[string]bool{}
	if path == "" {
		return m, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Fields(string(b)) {
		m[l] = true
	}
	return m, nil
}

func label(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("label", flag.ExitOnError)
	repoDir := fs.String("repo", ".", "repository")
	name := fs.String("name", "", "repository name for reports")
	since := fs.String("since", "", "first day (YYYY-MM-DD)")
	ciFailed := fs.String("ci-failed", "", "file of commit IDs whose CI failed")
	fixAnswers := fs.String("fix-answers", "", "answers to fixq requests")
	threshold := fs.Float64("fix-threshold", 0.7, "model probability above which a commit is a fix")
	out := fs.String("out", "", "output JSONL")
	fs.Parse(args)
	t, err := parseDate(*since)
	if err != nil {
		return err
	}
	repo, err := gitx.Open(ctx, *repoDir)
	if err != nil {
		return err
	}
	ci, err := readSet(*ciFailed)
	if err != nil {
		return err
	}
	runs, err := eval.LoadRuns(*fixAnswers)
	if err != nil {
		return err
	}
	labels, err := eval.Labels(ctx, repo, eval.LabelOptions{
		Repo: *name, Since: t, CIFailed: ci, FixThreshold: *threshold,
		IsFix: func(sha string) (float64, bool) {
			rs := runs["fix:"+sha]
			if len(rs) == 0 || rs[0].Answers["is_fix"].Noul == nil {
				return 0, false
			}
			return *rs[0].Answers["is_fix"].Noul, true
		},
	})
	if err != nil {
		return err
	}
	var bad, rev, szz, ci2, cens, fixes int
	for _, l := range labels {
		if l.Censored {
			cens++
			continue
		}
		if l.Bad {
			bad++
		}
		if l.Reverted {
			rev++
		}
		if len(l.FixedBy) > 0 {
			szz++
		}
		if l.CIFailed {
			ci2++
		}
		if l.IsFix {
			fixes++
		}
	}
	fmt.Fprintf(os.Stderr, "%s: %d commits, %d censored; of the rest %d bad (%d reverted, %d fixed within 14 days, %d CI failed); %d are fixes\n",
		*name, len(labels), cens, bad, rev, szz, ci2, fixes)
	return eval.WriteJSONL(*out, labels)
}

func prepare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	repoDir := fs.String("repo", ".", "repository")
	labelsPath := fs.String("labels", "", "labels JSONL")
	out := fs.String("out", "", "records JSONL")
	reqOut := fs.String("requests", "", "requests JSONL for the answering plugin")
	repeat := fs.Int("repeat", 5, "runs per request for the stability sample")
	sample := fs.Int("repeat-sample", 200, "how many requests to repeat")
	repeatOut := fs.String("repeat-out", "", "requests JSONL for the extra stability runs")
	workers := fs.Int("workers", 2, "commits prepared at once (each analyses up to 8 files at once)")
	fs.Parse(args)
	labels, err := eval.ReadJSONL[eval.Label](*labelsPath)
	if err != nil {
		return err
	}
	repo, err := gitx.Open(ctx, *repoDir)
	if err != nil {
		return err
	}
	prov := &gitctx.Provider{}
	// Load history once from HEAD so every older base reuses it.
	head, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		return err
	}
	if _, err := prov.Context(ctx, risk.ContextQuery{RepoRoot: repo.Root, Base: head}); err != nil {
		return err
	}
	eng := &assess.Engine{Repo: repo, Lang: languageBackends(), Context: prov}
	start := time.Now()
	recs, lines, err := eval.Prepare(ctx, eng, labels, *workers)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prepared %d commits in %s, %d requests\n", len(recs), time.Since(start).Round(time.Millisecond), len(lines))
	if err := eval.WriteJSONL(*out, recs); err != nil {
		return err
	}
	keys := make([]string, 0, len(lines))
	for k := range lines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var reqs []eval.HostLine
	for _, k := range keys {
		reqs = append(reqs, lines[k])
	}
	if err := eval.WriteJSONL(*reqOut, reqs); err != nil {
		return err
	}
	if *repeatOut != "" {
		// A fixed-seed sample of uncensored requests, run repeat-1 more times.
		var pool []string
		for _, r := range recs {
			if !r.Censored && r.RequestID != "" {
				pool = append(pool, r.RequestID)
			}
		}
		sort.Strings(pool)
		rng := rand.New(rand.NewPCG(42, 42))
		rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		pool = pool[:min(*sample, len(pool))]
		var extra []eval.HostLine
		for _, id := range pool {
			for k := 2; k <= *repeat; k++ {
				l := lines[id]
				l.Key = fmt.Sprintf("%s#%d", id, k)
				extra = append(extra, l)
			}
		}
		if err := eval.WriteJSONL(*repeatOut, extra); err != nil {
			return err
		}
	}
	return nil
}

// answer answers requests through OpenRouter, writing the same format as
// the eval/ampdecide plugin.
func answer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	in := fs.String("in", "", "requests JSONL")
	out := fs.String("out", "", "answers JSONL (appended; done keys are skipped)")
	workers := fs.Int("workers", 8, "parallel requests")
	fs.Parse(args)
	b := openrouter.FromEnv()
	if b == nil {
		return fmt.Errorf("OPENROUTER_API_KEY is not set")
	}
	reqs, err := eval.ReadJSONL[eval.HostLine](*in)
	if err != nil {
		return err
	}
	done, err := eval.LoadRuns(*out)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, rs := range done {
		for _, r := range rs {
			seen[r.Key] = true
		}
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	type result struct {
		eval.Run
		InputTokens int     `json:"input_tokens"`
		CostUSD     float64 `json:"cost_usd"`
	}
	ch := make(chan eval.HostLine)
	res := make(chan result)
	for w := 0; w < *workers; w++ {
		go func() {
			for l := range ch {
				t := time.Now()
				resp, err := b.Answer(ctx, risk.Request{State: l.State, Questions: l.Questions})
				r := result{Run: eval.Run{Key: l.Key, Request: l.RequestID, MS: float64(time.Since(t).Microseconds()) / 1000}}
				if err != nil {
					r.Error = err.Error()
				} else {
					r.Model, r.Answers = resp.Model, resp.Answers
					if resp.Usage != nil {
						r.InputTokens, r.CostUSD = resp.Usage.InputTokens, resp.Usage.CostUSD
					}
				}
				res <- r
			}
		}()
	}
	todo := 0
	go func() {
		for _, l := range reqs {
			if !seen[l.Key] {
				ch <- l
			}
		}
		close(ch)
	}()
	for _, l := range reqs {
		if !seen[l.Key] {
			todo++
		}
	}
	enc := json.NewEncoder(f)
	failed := 0
	for i := 0; i < todo; i++ {
		r := <-res
		if r.Error != "" {
			failed++
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "answered %d, failed %d\n", todo-failed, failed)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Report is the evaluation's JSON output.
type Report struct {
	Generated   time.Time                 `json:"generated"`
	Calibration *calibration.Table        `json:"calibration"`
	Models      map[string]int            `json:"models"`
	Split       float64                   `json:"split"`
	Test        map[string]eval.Summary   `json:"test"`
	All         map[string]eval.Summary   `json:"all_in_sample"`
	RulesOnly   map[string]eval.Summary   `json:"rules_only_test"`
	AmpMade     eval.Summary              `json:"amp_made_test"`
	Labels      map[string]map[string]int `json:"labels"`
	Stability   map[string]any            `json:"stability"`
	Latency     map[string]float64        `json:"latency_ms"`
	Cost        map[string]float64        `json:"cost"`
	HitRates    map[string]map[string]int `json:"rule_hits"`
	Presets     map[string]PresetSummary  `json:"presets"`
}

// PresetSummary is one policy preset on the test set.
type PresetSummary struct {
	eval.Summary
	// HumanShare is the share of changes where human_check asks for a
	// person; HumanRecall the share of bad changes it asks about.
	HumanShare     eval.Proportion `json:"human_check_share"`
	HumanRecall    eval.Proportion `json:"human_check_recall"`
	BadRateHuman   eval.Proportion `json:"bad_rate_human_check"`
	BadRateNoHuman eval.Proportion `json:"bad_rate_no_human_check"`
	HumanBelowHigh int             `json:"human_check_below_high"`
}

func score(args []string) error {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	recPaths := fs.String("records", "", "comma-separated records JSONL")
	ansPaths := fs.String("answers", "", "comma-separated answers JSONL")
	split := fs.Float64("split", 0.6, "share of each repo's history, oldest first, used to fit the calibration")
	fitOut := fs.String("fit-out", "", "write the fitted calibration table here")
	tablePath := fs.String("table", "", "use this calibration table instead of fitting")
	highBudget := fs.Float64("high-budget", 0.12, "share of training changes allowed in High")
	medShare := fs.Float64("medium-share", 0.40, "share of training changes at Medium or above")
	reportPath := fs.String("report", "", "report JSON")
	latPath := fs.String("latency-answers", "", "answers from sequential calls, for model latency")
	features := fs.String("features", "", "comma-separated features to fit (ablation); default all")
	presets := fs.String("presets", "light,balanced,careful", "comma-separated policy presets to report on the test set")
	quantilesOut := fs.String("quantiles-out", "", "with -table: write the table here with risk_quantiles from the training split")
	fs.Parse(args)

	var recs []eval.Record
	for _, p := range splitList(*recPaths) {
		rs, err := eval.ReadJSONL[eval.Record](p)
		if err != nil {
			return err
		}
		recs = append(recs, rs...)
	}
	runs, err := eval.LoadRuns(splitList(*ansPaths)...)
	if err != nil {
		return err
	}

	byRepo := map[string][]eval.Record{}
	for _, r := range recs {
		if r.Censored || r.RequestID == "" {
			continue
		}
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	var train, test []eval.Record
	testByRepo := map[string][]eval.Record{}
	repos := []string{}
	for repo, rs := range byRepo {
		repos = append(repos, repo)
		sort.Slice(rs, func(i, j int) bool { return rs[i].When.Before(rs[j].When) })
		cut := int(*split * float64(len(rs)))
		train = append(train, rs[:cut]...)
		test = append(test, rs[cut:]...)
		testByRepo[repo] = rs[cut:]
	}
	sort.Strings(repos)

	models := map[string]int{}
	for _, rs := range runs {
		for _, r := range rs {
			models[r.Model]++
		}
	}
	var modelIDs []string
	for m := range models {
		modelIDs = append(modelIDs, calibration.Normalise(m))
	}

	var table *calibration.Table
	if *tablePath != "" {
		b, err := os.ReadFile(*tablePath)
		if err != nil {
			return err
		}
		table = &calibration.Table{}
		if err := json.Unmarshal(b, table); err != nil {
			return err
		}
	} else {
		id := "jev-1.14"
		if *fitOut != "" {
			id = strings.TrimSuffix(filepath.Base(*fitOut), ".json")
		}
		// Overrides are policy, not fitted: keep the embedded table's, or
		// the default table's for a new ID.
		var overrides []calibration.Override
		if old, ok := calibration.ByID(id); ok {
			overrides = old.Overrides
		} else if def, ok := calibration.ByID("default"); ok {
			overrides = def.Overrides
		}
		table = eval.Fit(train, runs, eval.FitOptions{
			Overrides: overrides,
			ID:        id, Models: uniq(modelIDs), HighBudget: *highBudget, MediumShare: *medShare, Features: splitList(*features),
			FittedOn: fmt.Sprintf("%d changes: the oldest %.0f%% of %s", len(train), *split*100, strings.Join(repos, ", ")),
		})
		table.RiskQuantiles = eval.RiskQuantiles(train, runs, table)
		table.Notes = "Fitted by brisk-eval score. Features: impact and uncertainty are Jev scores / 3, flags is the largest of the four yes/no risk answers, follow_up is the follow_up_fix answer, history is prior fixes to the changed files."
		if *fitOut != "" {
			b, _ := json.MarshalIndent(table, "", "  ")
			if err := os.WriteFile(*fitOut, append(b, '\n'), 0o644); err != nil {
				return err
			}
		}
	}

	if *quantilesOut != "" {
		withQ := *table
		withQ.RiskQuantiles = eval.RiskQuantiles(train, runs, table)
		b, _ := json.MarshalIndent(&withQ, "", "  ")
		if err := os.WriteFile(*quantilesOut, append(b, '\n'), 0o644); err != nil {
			return err
		}
		table = &withQ
	}

	rep := Report{Generated: time.Now().UTC(), Calibration: table, Models: models, Split: *split,
		Test: map[string]eval.Summary{}, All: map[string]eval.Summary{}, RulesOnly: map[string]eval.Summary{},
		Labels: map[string]map[string]int{}, Latency: map[string]float64{}, Cost: map[string]float64{}, HitRates: map[string]map[string]int{}}
	rulesOnly := func(rs []eval.Record) []eval.Scored { return eval.Score(rs, nil, table, 0) }
	var allRecs []eval.Record
	for _, repo := range repos {
		allRecs = append(allRecs, byRepo[repo]...)
		rep.Test[repo] = eval.Summarise(eval.Score(testByRepo[repo], runs, table, 0))
		rep.All[repo] = eval.Summarise(eval.Score(byRepo[repo], runs, table, 0))
		rep.RulesOnly[repo] = eval.Summarise(rulesOnly(testByRepo[repo]))
		lc := map[string]int{"changes": len(byRepo[repo])}
		hits := map[string]int{}
		for _, r := range byRepo[repo] {
			if r.Bad {
				lc["bad"]++
			}
			if r.Reverted {
				lc["reverted"]++
			}
			if len(r.FixedBy) > 0 {
				lc["fixed_within_14d"]++
			}
			if r.CIFailed {
				lc["ci_failed"]++
			}
			if r.AmpMade {
				lc["amp_made"]++
			}
			for _, h := range r.Hits {
				hits[h]++
			}
		}
		rep.Labels[repo] = lc
		rep.HitRates[repo] = hits
	}
	rep.Presets = map[string]PresetSummary{}
	for _, name := range splitList(*presets) {
		if n, ok := assess.PresetAliases[name]; ok {
			name = n
		}
		pol, ok := assess.Presets[name]
		if !ok {
			return fmt.Errorf("unknown preset %q", name)
		}
		ss, err := eval.ScorePolicy(test, runs, table, pol)
		if err != nil {
			return err
		}
		ps := PresetSummary{Summary: eval.Summarise(ss)}
		var rec, recBad, bad, other, otherBad, recMedLow int
		for _, x := range ss {
			if x.Bad {
				bad++
			}
			if x.NeedsHuman {
				rec++
				if x.Bad {
					recBad++
				}
				if x.Tier < risk.High {
					recMedLow++
				}
			} else {
				other++
				if x.Bad {
					otherBad++
				}
			}
		}
		ps.HumanShare = eval.NewProportion(rec, len(ss))
		ps.HumanRecall = eval.NewProportion(recBad, bad)
		ps.BadRateHuman = eval.NewProportion(recBad, rec)
		ps.BadRateNoHuman = eval.NewProportion(otherBad, other)
		ps.HumanBelowHigh = recMedLow
		rep.Presets[name] = ps
	}
	rep.Test["combined"] = eval.Summarise(eval.Score(test, runs, table, 0))
	rep.All["combined"] = eval.Summarise(eval.Score(allRecs, runs, table, 0))
	rep.RulesOnly["combined"] = eval.Summarise(rulesOnly(test))
	var amp []eval.Record
	for _, r := range test {
		if r.AmpMade {
			amp = append(amp, r)
		}
	}
	rep.AmpMade = eval.Summarise(eval.Score(amp, runs, table, 0))

	// Stability: requests with at least 5 runs.
	flips, n := 0, 0
	riskSpread := []float64{}
	for _, r := range allRecs {
		rs := runs[r.RequestID]
		if len(rs) < 5 {
			continue
		}
		n++
		tiers := map[risk.Tier]bool{}
		lo, hi := 2.0, -1.0
		for k := 0; k < 5; k++ {
			s := eval.Score([]eval.Record{r}, runs, table, k)[0]
			tiers[s.Tier] = true
			lo, hi = min(lo, s.ModelRisk), max(hi, s.ModelRisk)
		}
		if len(tiers) > 1 {
			flips++
		}
		riskSpread = append(riskSpread, hi-lo)
	}
	rep.Stability = map[string]any{"changes": n, "tier_flips": eval.NewProportion(flips, n)}
	if n > 0 { // JSON has no NaN: leave the ranges out without a sample
		rep.Stability["model_risk_range_p50"] = eval.Percentile(riskSpread, 50)
		rep.Stability["model_risk_range_p95"] = eval.Percentile(riskSpread, 95)
	}

	// Latency: prepare time per change, plus the model time of sequential
	// calls (batch answers run many at once and overstate it).
	lat := runs
	if *latPath != "" {
		if lat, err = eval.LoadRuns(*latPath); err != nil {
			return err
		}
	}
	var prep, model, total, chars []float64
	for _, r := range allRecs {
		prep = append(prep, r.PrepareMS)
		chars = append(chars, float64(r.StateChars))
		if rs := lat[r.RequestID]; len(rs) > 0 {
			model = append(model, rs[0].MS)
			total = append(total, r.PrepareMS+rs[0].MS)
		}
	}
	rep.Latency["model_samples"] = float64(len(model))
	for name, xs := range map[string][]float64{"prepare": prep, "model": model, "total": total} {
		rep.Latency[name+"_p50"] = eval.Percentile(xs, 50)
		rep.Latency[name+"_p95"] = eval.Percentile(xs, 95)
	}
	meanChars := 0.0
	for _, c := range chars {
		meanChars += c
	}
	meanChars /= float64(max(1, len(chars)))
	// Jev bills input tokens only, at $0.042 per million. Tokens are
	// estimated at 3.5 characters each until OpenRouter usage is available.
	rep.Cost["mean_state_chars"] = meanChars
	rep.Cost["est_input_tokens"] = meanChars / 3.5
	rep.Cost["est_usd_per_assessment"] = meanChars / 3.5 * 0.042 / 1e6

	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if *reportPath != "" {
		if err := os.WriteFile(*reportPath, b, 0o644); err != nil {
			return err
		}
	}
	printSummary(rep, repos)
	return nil
}

func printSummary(rep Report, repos []string) {
	fmt.Printf("calibration %s weights %v thresholds medium %.3f high %.3f\n", rep.Calibration.ID, rep.Calibration.Weights, rep.Calibration.Thresholds.Medium, rep.Calibration.Thresholds.High)
	for _, k := range append(repos, "combined") {
		s := rep.Test[k]
		ro := rep.RulesOnly[k]
		fmt.Printf("%-11s test n=%d bad=%d answered=%d | High recall %.2f [%.2f,%.2f] flag %.2f | bad rate L/M/H %.3f/%.3f/%.3f (n %d/%d/%d) mono=%v | AP brisk %.3f rules %.3f size %.3f model %.3f base %.3f | Δ %.3f [%.3f,%.3f] | rules-only High recall %.2f flag %.2f\n",
			k, s.N, s.Bad, s.Answered, s.HighRecall.Value, s.HighRecall.Low, s.HighRecall.High, s.HighFlag.Value,
			s.BadRate["low"].Value, s.BadRate["medium"].Value, s.BadRate["high"].Value, s.TierCounts["low"], s.TierCounts["medium"], s.TierCounts["high"], s.Monotonic,
			s.AP["brisk"], s.AP["rules_only"], s.AP["diff_size"], s.AP["model_only"], s.AP["base_rate"],
			s.APDiffRules.Value, s.APDiffRules.Low, s.APDiffRules.High, ro.HighRecall.Value, ro.HighFlag.Value)
	}
	names := make([]string, 0, len(rep.Presets))
	for n := range rep.Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := rep.Presets[n]
		fmt.Printf("preset %-10s High flag %.2f recall %.2f [%.2f,%.2f] | Medium+ flag %.2f recall %.2f | bad rate L/M/H %.3f/%.3f/%.3f | person asked %.2f recall %.2f [%.2f,%.2f], bad rate %.3f vs %.3f without, %d below High\n",
			n, p.HighFlag.Value, p.HighRecall.Value, p.HighRecall.Low, p.HighRecall.High,
			float64(p.TierCounts["medium"]+p.TierCounts["high"])/float64(max(1, p.N)), p.MedHighRec.Value,
			p.BadRate["low"].Value, p.BadRate["medium"].Value, p.BadRate["high"].Value,
			p.HumanShare.Value, p.HumanRecall.Value, p.HumanRecall.Low, p.HumanRecall.High,
			p.BadRateHuman.Value, p.BadRateNoHuman.Value, p.HumanBelowHigh)
	}
	fmt.Printf("stability %v\nlatency %v\ncost %v\nlabels %v\n", rep.Stability, rep.Latency, rep.Cost, rep.Labels)
}

func uniq(xs []string) []string {
	sort.Strings(xs)
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
