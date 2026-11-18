// Package assess is brisk's engine: it collects a change, applies rules and
// language backends, asks a backend the typed questions, and turns the
// answers into a tier that is never below the rule floor.
package assess

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ronny/brisk/backend/host"
	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
	"github.com/ronny/brisk/rules"
)

// Version is the release tag, a semantic version such as v0.1.0, set at build time
// with -ldflags "-X github.com/ronny/brisk/assess.Version=<tag>". Other
// builds report dev plus the commit they were built from.
var Version = "dev"

func init() {
	if Version != "dev" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && len(s.Value) >= 12:
			rev = s.Value[:12]
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = "-dirty"
		}
	}
	switch {
	case rev != "":
		Version = "dev-" + rev + dirty
	case bi.Main.Version != "" && bi.Main.Version != "(devel)":
		// go install github.com/ronny/brisk/cmd/brisk@<tag> has no VCS settings.
		Version = bi.Main.Version
	}
}

// Engine holds the parts that do not change between assessments.
type Engine struct {
	Repo    *gitx.Repo
	Lang    []risk.LanguageBackend
	Context risk.ContextProvider
}

// Input describes one assessment.
type Input struct {
	Base    string // commit or ref
	Head    string // commit or ref; "" for the working tree
	Session *risk.Session
	// Task is used as the task description when there is no session
	// intent, e.g. the commit message in offline evaluation.
	Task string
}

// Prepared is everything up to the model call. Finish turns it into an
// assessment with any backend's answers, so evaluation can reuse it.
type Prepared struct {
	Changes   *risk.ChangeSet
	Findings  []risk.Finding
	Context   risk.ProjectContext
	Hits      []rules.Hit
	Floor     risk.Tier
	Request   *risk.Request // nil when there is nothing to ask
	RequestID string        // host.Key(Request): recorded answers are keyed by it
	// ReviewQuestions score the review plan's areas, asked on the same
	// state in a separate request so the calibrated request is unchanged.
	ReviewQuestions map[string]risk.Question
	// ReviewAnswers are the answers to ReviewQuestions, when there are any.
	ReviewAnswers map[string]risk.Answer
	// ReviewError says why the review call failed; ReviewUsage is its cost.
	ReviewError string
	ReviewUsage *risk.Usage
	Policy      Policy
	Timings     map[string]float64
	started     time.Time
}

// Prepare collects the change and everything rules can decide.
func (e *Engine) Prepare(ctx context.Context, in Input) (*Prepared, error) {
	p := &Prepared{started: time.Now(), Timings: map[string]float64{}}
	lap := func(name string, t time.Time) { p.Timings[name] = float64(time.Since(t).Microseconds()) / 1000 }

	t := time.Now()
	cs, err := e.Repo.Changes(ctx, in.Base, in.Head)
	if err != nil {
		return nil, err
	}
	for i := range cs.Files {
		cs.Files[i].HandEdited = rules.HandEditedGenerated(cs.Files[i], in.Session, cs.RepoRoot)
	}
	p.Changes = cs
	lap("git", t)

	t = time.Now()
	p.Findings = e.analyze(ctx, cs)
	lap("language", t)

	t = time.Now()
	if e.Context != nil {
		paths := make([]string, 0, len(cs.Files))
		for _, f := range cs.Files {
			paths = append(paths, f.Path)
		}
		pc, err := e.Context.Context(ctx, risk.ContextQuery{RepoRoot: cs.RepoRoot, Base: cs.Base, Paths: paths})
		if err != nil {
			return nil, fmt.Errorf("context: %w", err)
		}
		p.Context = pc
	}
	lap("context", t)

	if p.Policy, err = ResolvePolicy(p.Context.Policy); err == nil {
		err = p.Policy.CheckIDs(rules.KnownIDs(), projectIDs(p.Context))
	}
	if err != nil {
		return nil, fmt.Errorf(".brisk.yaml: %w", err)
	}
	p.Hits = rules.Apply(cs, p.Findings, in.Session, rules.Config{PathRules: p.Context.SensitivePaths, ContentRules: p.Context.ContentRules})
	// Editing the business-rules file is a change to brisk's own rules,
	// wherever the team keeps it.
	if br := p.Context.BusinessRules; br != nil && !slices.ContainsFunc(p.Hits, func(h rules.Hit) bool { return h.ID == "brisk_config" }) {
		for _, f := range cs.Files {
			if f.Path == br.Path || f.OldPath == br.Path {
				p.Hits = append(p.Hits, rules.Hit{ID: "brisk_config", Tier: risk.Medium, Text: "Changes brisk's own risk rules", Source: "rule", Evidence: []risk.Evidence{{Path: f.Path}}})
				break
			}
		}
	}
	for i, h := range p.Hits {
		// A project rule hits as config:<id>; its id may be written either way.
		if t, ok := p.Policy.RuleTiers[h.ID]; ok {
			p.Hits[i].Tier = t
		} else if id, ok := strings.CutPrefix(h.ID, "config:"); ok {
			if t, ok := p.Policy.RuleTiers[id]; ok {
				p.Hits[i].Tier = t
			}
		}
	}
	p.Floor = rules.Floor(p.Hits)

	if len(cs.Files) > 0 {
		req := risk.Request{
			State:     buildState(cs, p.Findings, p.Hits, p.Context, in.Session, in.Task),
			Questions: p.Policy.RequestQuestions(),
		}
		id, err := host.Key(req)
		if err != nil {
			return nil, err
		}
		p.Request, p.RequestID = &req, id
		if p.Policy.Tailor() != "never" {
			p.ReviewQuestions = reviewQuestions(p.planCandidates())
		}
	}
	return p, nil
}

// unmarkGenerated lowercases the generated-code markers in src.
func unmarkGenerated(src []byte) []byte {
	if src == nil {
		return nil
	}
	return []byte(strings.NewReplacer("DO NOT EDIT", "do not edit", "@generated", "@Generated").Replace(string(src)))
}

func projectIDs(pc risk.ProjectContext) []string {
	var ids []string
	for _, r := range pc.SensitivePaths {
		ids = append(ids, r.ID)
	}
	for _, r := range pc.ContentRules {
		ids = append(ids, r.ID)
	}
	return ids
}

// analyze runs the language backends over changed files in parallel.
func (e *Engine) analyze(ctx context.Context, cs *risk.ChangeSet) []risk.Finding {
	if len(e.Lang) == 0 {
		return nil
	}
	// No shared deadline: a deadline that some files miss on a busy
	// machine would make the findings, the floor, and the request ID depend
	// on timing. Each file gets a generous limit only to stop a hang.
	var mu sync.Mutex
	var out []risk.Finding
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, f := range cs.Files {
		if f.Binary || rules.IsGeneratedChange(f) || f.Status == risk.Deleted && len(f.Old) == 0 {
			continue
		}
		if f.HandEdited {
			// Language backends skip files that say they are generated;
			// show them a copy whose marker no longer says so. Only
			// comment text changes, so lines and code stay the same.
			f.Old, f.New = unmarkGenerated(f.Old), unmarkGenerated(f.New)
		}
		for _, lb := range e.Lang {
			if !lb.Supports(f.Path) {
				continue
			}
			wg.Add(1)
			go func(lb risk.LanguageBackend, f risk.FileChange) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				defer func() {
					if r := recover(); r != nil {
						mu.Lock()
						out = append(out, risk.Finding{Kind: risk.ParseError, Path: f.Path, Detail: fmt.Sprint("panic: ", r), Language: lb.Name()})
						mu.Unlock()
					}
				}()
				fctx, cancel := context.WithTimeout(ctx, FileTimeout)
				defer cancel()
				fs, err := lb.Analyze(fctx, f)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					out = append(out, risk.Finding{Kind: risk.ParseError, Path: f.Path, Detail: err.Error(), Language: lb.Name()})
					return
				}
				out = append(out, fs...)
			}(lb, f)
			break
		}
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.StartLine != b.StartLine:
			return a.StartLine < b.StartLine
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		case a.EndLine != b.EndLine:
			return a.EndLine < b.EndLine
		case a.Symbol != b.Symbol:
			return a.Symbol < b.Symbol
		case a.Language != b.Language:
			return a.Language < b.Language
		}
		return a.Detail < b.Detail
	})
	return out
}

// FileTimeout bounds language analysis of one file.
var FileTimeout = 20 * time.Second

// Run prepares and answers with backend b. When b fails, the assessment is
// rules only and says why in provenance.degraded.
func (e *Engine) Run(ctx context.Context, in Input, b risk.Backend) (risk.Assessment, error) {
	p, err := e.Prepare(ctx, in)
	if err != nil {
		return risk.Assessment{}, err
	}
	return p.Answer(ctx, b), nil
}

// Answer asks b the questions and finishes the assessment.
func (p *Prepared) Answer(ctx context.Context, b risk.Backend) risk.Assessment {
	if b == nil || p.Request == nil {
		return p.Finish("rules-only", nil, "")
	}
	// With tailor_plan always (the default) the review questions go out
	// alongside the main ones; they are a separate call on the same state.
	type result struct {
		r   risk.Response
		err error
		ms  float64
	}
	review := func() result {
		t := time.Now()
		r, err := b.Answer(ctx, *p.ReviewRequest())
		return result{r, err, float64(time.Since(t).Microseconds()) / 1000}
	}
	var early chan result
	if p.Policy.Tailor() == "always" && p.ReviewRequest() != nil {
		early = make(chan result, 1)
		go func() { early <- review() }()
	}
	t := time.Now()
	resp, err := b.Answer(ctx, *p.Request)
	p.Timings["model"] = float64(time.Since(t).Microseconds()) / 1000
	if err == risk.ErrNoModel {
		return p.Finish("rules-only", nil, "")
	}
	if err != nil {
		return p.Finish("rules-only", nil, b.Name()+": "+err.Error())
	}
	a := p.Finish(b.Name(), &resp, "")
	// Otherwise they are asked only when the policy wants them for this
	// assessment (tailor_plan needed: when a person is asked). Without
	// their answers the plan is brisk's alone, which is not an error.
	var r result
	switch {
	case early != nil:
		r = <-early
	case p.WantsReviewAnswers(a):
		r = review()
	default:
		return a
	}
	p.Timings["review_model"] = r.ms
	switch {
	case r.err != nil:
		p.ReviewError = b.Name() + ": " + r.err.Error()
	case !answersAll(r.r.Answers, p.ReviewQuestions):
		p.ReviewError = b.Name() + ": the review answers do not answer every review question"
	default:
		p.ReviewAnswers, p.ReviewUsage = r.r.Answers, r.r.Usage
	}
	return p.Finish(b.Name(), &resp, "")
}

// WantsReviewAnswers reports whether the review questions should be asked
// for assessment a, made from the main answers.
func (p *Prepared) WantsReviewAnswers(a risk.Assessment) bool {
	if p.ReviewRequest() == nil {
		return false
	}
	switch p.Policy.Tailor() {
	case "always":
		return true
	case "needed":
		return a.HumanCheck.Needed
	}
	return false
}

// ReviewRequest is the request that scores the review plan, or nil.
func (p *Prepared) ReviewRequest() *risk.Request {
	if p.Request == nil || len(p.ReviewQuestions) == 0 {
		return nil
	}
	return &risk.Request{State: p.Request.State, Questions: p.ReviewQuestions}
}

// History is the 0–1 history feature: prior fixes and reverts to the
// changed files, on a log scale that saturates at 20.
func (p *Prepared) History() float64 {
	maxFix := 0
	for _, h := range p.Context.Files {
		maxFix = max(maxFix, h.FixCommits+2*h.RevertCommits)
	}
	return math.Min(1, math.Log1p(float64(maxFix))/math.Log1p(20))
}

// SizeFeature is the 0–1 size feature: changed lines on a log scale that
// saturates at 2,000.
func SizeFeature(lines int) float64 {
	return math.Min(1, math.Log1p(float64(lines))/math.Log1p(2000))
}

// Features turns answers, the history feature, and the size feature into
// the calibration's features.
func Features(answers map[string]risk.Answer, history, size float64) map[string]float64 {
	f := map[string]float64{}
	// Clamp: host answers come from outside and may be out of range.
	clamp := func(x float64) float64 { return math.Min(1, math.Max(0, x)) }
	score := func(name string) float64 {
		if a, ok := answers[name]; ok && a.Score != nil {
			return clamp(*a.Score / 3)
		}
		return 0
	}
	f["impact"] = score("impact")
	f["uncertainty"] = score("uncertainty")
	f["impact_x_uncertainty"] = f["impact"] * f["uncertainty"]
	// Only brisk's own questions are calibrated; a project's questions act
	// through overrides alone.
	for name := range flagText {
		if a := answers[name]; a.Noul != nil && clamp(*a.Noul) > f["flags"] {
			f["flags"] = clamp(*a.Noul)
		}
	}
	if a := answers["follow_up_fix"]; a.Noul != nil {
		f["follow_up"] = clamp(*a.Noul)
	}
	f["history"] = history
	f["size"] = size
	return f
}

// ModelTier is the tier the model supports: from the calibrated risk, raised
// by any override whose answer is strong enough. It also returns the
// overrides that fired.
func ModelTier(t *calibration.Table, risk01 float64, answers map[string]risk.Answer) (risk.Tier, []calibration.Override) {
	tier := risk.Low
	switch {
	case risk01 >= t.Thresholds.High:
		tier = risk.High
	case risk01 >= t.Thresholds.Medium:
		tier = risk.Medium
	}
	var fired []calibration.Override
	for _, o := range t.Overrides {
		a, ok := answers[o.Question]
		if !ok {
			continue
		}
		v := a.Noul
		if a.Score != nil {
			v = a.Score
		}
		ot, err := risk.ParseTier(o.Tier)
		if v == nil || err != nil || *v < o.Min {
			continue
		}
		fired = append(fired, o)
		tier = risk.Max(tier, ot)
	}
	return tier, fired
}

// Finish builds the assessment. resp is nil for rules only.
func (p *Prepared) Finish(backend string, resp *risk.Response, degraded string) risk.Assessment {
	a := risk.Assessment{
		Schema: risk.AssessmentSchema,
		Floor:  p.Floor,
		Tier:   p.Floor,
		Provenance: risk.Provenance{
			BriskVersion: Version,
			Backend:      backend,
			Degraded:     degraded,
			Base:         p.Changes.Base,
			Head:         p.Changes.Head,
			DiffHash:     p.Changes.DiffHash,
			RuleHits:     []string{},
			Findings:     p.Findings,
			Timings:      p.Timings,
		},
	}
	for _, f := range p.Changes.Files {
		a.Stats.Files++
		a.Stats.Additions += f.Additions
		a.Stats.Deletions += f.Deletions
	}
	for _, h := range p.Hits {
		a.Provenance.RuleHits = append(a.Provenance.RuleHits, h.ID)
	}

	var candidates []risk.Reason
	for _, h := range p.Hits {
		if h.Tier == risk.Low {
			continue
		}
		candidates = append(candidates, risk.Reason{Text: h.Text, Source: h.Source, ID: h.ID, Tier: h.Tier, Evidence: h.Evidence})
	}

	var firedIDs []string
	firedTier := map[string]risk.Tier{}
	if resp != nil {
		a.Provenance.Model = resp.Model
		a.Provenance.Answers = resp.Answers
		a.Provenance.Usage = resp.Usage
		table, ok, err := calibration.For(resp.Model)
		if err == nil {
			a.Provenance.Calibration = table.ID
			if !ok {
				a.Provenance.Calibration += " (no table for " + calibration.Normalise(resp.Model) + ")"
			}
			table, err = p.Policy.Table(table)
			if err != nil {
				a.Provenance.PolicyNote = err.Error() + "; using the calibration's thresholds"
			}
			a.Provenance.Thresholds = map[string]float64{"medium": table.Thresholds.Medium, "high": table.Thresholds.High}
			feats := Features(resp.Answers, p.History(), SizeFeature(a.Stats.Additions+a.Stats.Deletions))
			r := table.Risk(feats)
			shown := math.Round(r*1000) / 1000 // rounded for display only
			a.Provenance.ModelRisk = &shown
			a.Impact = round2(feats["impact"] * 3)
			a.Uncertainty = round2(feats["uncertainty"] * 3)
			modelTier, fired := ModelTier(table, r, resp.Answers)
			a.Tier = risk.Max(p.Floor, modelTier)
			for _, o := range fired {
				t, _ := risk.ParseTier(o.Tier)
				if _, ok := firedTier[o.Question]; !ok {
					firedIDs = append(firedIDs, o.Question)
				}
				firedTier[o.Question] = risk.Max(firedTier[o.Question], t)
			}
			if modelTier > risk.Low {
				for _, c := range modelReasons(table, feats, r, resp.Answers, fired, a.Stats) {
					// Only a fired override speaks for a project question;
					// feature reasons such as size keep brisk's text.
					if slices.ContainsFunc(fired, func(o calibration.Override) bool { return o.Question == c.ID }) {
						c.Text = p.Policy.questionText(c.ID, c.Text, resp.Answers)
					}
					candidates = append(candidates, c)
				}
			}
		} else {
			a.Provenance.Degraded = "calibration: " + err.Error()
		}
	}

	// Strongest reasons first: those at the final tier, rules before model.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Tier > candidates[j].Tier })
	for _, c := range candidates {
		if len(a.Reasons) == 3 {
			break
		}
		a.Reasons = append(a.Reasons, c)
	}
	if a.Reasons == nil {
		a.Reasons = []risk.Reason{}
	}
	a.Action = p.Policy.Actions[a.Tier]
	a.HumanCheck.Needed, a.HumanCheck.Why, a.HumanCheck.Reason = p.Policy.NeedsHuman(a.Tier, a.Provenance.RuleHits, a.Provenance.Answers)
	var review map[string]risk.Answer
	if resp != nil {
		review = p.ReviewAnswers
	}
	a.AgentReview.Plan, a.Provenance.ReviewPlan = p.buildPlan(firedIDs, firedTier, a.Provenance.Answers, review)
	a.HumanCheck.Consider = consider(a.HumanCheck.Needed, a.AgentReview.Plan)
	if br := p.Context.BusinessRules; br != nil {
		a.AgentReview.ContextFiles = []string{br.Path}
	}
	if rp := a.Provenance.ReviewPlan; rp != nil && !rp.Tailored {
		switch {
		case resp == nil:
			rp.Status = "not_asked"
		case p.ReviewError != "":
			rp.Status, rp.Error = "failed", p.ReviewError
		case p.WantsReviewAnswers(a):
			rp.Status = "pending"
		default:
			rp.Status = "not_asked"
		}
	}
	if resp != nil && p.ReviewUsage != nil {
		u := risk.Usage{}
		if a.Provenance.Usage != nil {
			u = *a.Provenance.Usage
		}
		u.InputTokens += p.ReviewUsage.InputTokens
		u.CostUSD += p.ReviewUsage.CostUSD
		a.Provenance.Usage = &u
	}
	a.Provenance.Preset = p.Policy.Preset
	a.Provenance.DurationMS = time.Since(p.started).Milliseconds()
	return a
}

// modelReasons explains the model's part of the tier with only what raised
// it: each override that fired, and, when the weighted risk alone reached a
// tier, the features that make up at least a quarter of that risk. Answers
// that changed nothing, such as a yes/no answer below its override, are not
// shown; they stay in provenance.answers.
func modelReasons(t *calibration.Table, feats map[string]float64, risk01 float64, answers map[string]risk.Answer, fired []calibration.Override, stats risk.Stats) []risk.Reason {
	var out []risk.Reason
	seen := map[string]bool{}
	for _, o := range fired {
		if seen[o.Question] {
			continue
		}
		seen[o.Question] = true
		tier, _ := risk.ParseTier(o.Tier)
		out = append(out, risk.Reason{Text: answerText(o.Question, answers), Source: "question", ID: o.Question, Tier: tier})
	}
	riskTier, _ := ModelTier(&calibration.Table{Thresholds: t.Thresholds}, risk01, nil)
	if riskTier == risk.Low || risk01 <= 0 {
		return out
	}
	type part struct {
		name  string
		share float64
	}
	var parts []part
	for name, w := range t.Weights {
		if c := w * feats[name]; c >= 0.25*risk01 {
			parts = append(parts, part{name, c / risk01})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].share > parts[j].share })
	add := func(id, text string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, risk.Reason{Text: text, Source: "question", ID: id, Tier: riskTier})
		}
	}
	for _, p := range parts {
		switch p.name {
		case "size":
			add("size", fmt.Sprintf("Large change: +%d -%d lines across %d files", stats.Additions, stats.Deletions, stats.Files))
		case "follow_up":
			add("follow_up_fix", answerText("follow_up_fix", answers))
		case "history":
			add("history", "The changed files were fixed or reverted often before")
		case "impact", "uncertainty":
			add(p.name, answerText(p.name, answers))
		case "impact_x_uncertainty":
			add("impact", answerText("impact", answers))
			add("uncertainty", answerText("uncertainty", answers))
		case "flags":
			best, v := "", 0.0
			for n := range flagText {
				if x := noul(answers[n]); x > v || x == v && n < best {
					best, v = n, x
				}
			}
			if best != "" {
				add(best, answerText(best, answers))
			}
		}
	}
	return out
}

// answerText describes one answer for a reason.
func answerText(q string, answers map[string]risk.Answer) string {
	a := answers[q]
	switch {
	case q == "impact" && a.Score != nil:
		return fmt.Sprintf("High impact if wrong (model: %.1f of 3)", *a.Score)
	case q == "uncertainty" && a.Score != nil:
		return fmt.Sprintf("Weakly verified (model: %.1f of 3)", *a.Score)
	case flagText[q] != "":
		return fmt.Sprintf("%s (model: %.2f)", flagText[q], noul(a))
	case q == "follow_up_fix":
		return fmt.Sprintf("%s (model: %.2f)", followUpText, noul(a))
	}
	return q
}

func noul(a risk.Answer) float64 {
	if a.Noul == nil {
		return 0
	}
	return *a.Noul
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// answersAll reports whether ans answers every question in qs.
func answersAll(ans map[string]risk.Answer, qs map[string]risk.Question) bool {
	for q := range qs {
		if _, ok := ans[q]; !ok {
			return false
		}
	}
	return true
}
