package assess

import (
	"fmt"
	"slices"
	"sort"

	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/risk"
)

// Policy is a team's risk appetite: how much the model may flag, what each
// tier asks for, and when to recommend an agent review. It comes from a
// preset, adjusted by .brisk.yaml from the base commit.
type Policy struct {
	Preset      string
	HighShare   *float64                // share of changes the model alone puts at High
	MediumShare *float64                // share at Medium or above
	Thresholds  *calibration.Thresholds // explicit; wins over the shares
	Overrides   []calibration.Override  // nil keeps the calibration's
	RuleTiers   map[string]risk.Tier
	Actions     map[risk.Tier]string
	Review      ReviewPolicy
}

// ReviewPolicy says when to recommend an agent review. Any one condition
// is enough.
type ReviewPolicy struct {
	ByTier         bool
	MinTier        risk.Tier
	MinUncertainty float64            // Jev's uncertainty score, 0–3; 0 is off
	Questions      map[string]float64 // yes/no answer minimums
	Rules          []string           // hit IDs
}

// DefaultPreset is used when .brisk.yaml names none.
const DefaultPreset = "reviewed"

// dangerRules are rules whose hits always deserve a second look.
var dangerRules = []string{"secret_added", "tls_disabled", "gitignore_hides_code", "nested_repo", "generated_without_source", "generated_hand_edited"}

func f(x float64) *float64 { return &x }

// Presets are the named starting points. The names match the Amp plugin's
// amp.brisk.preset setting, so a team picks one name everywhere.
//
//   - autonomous: nobody reviews changes by hand. High asks for an agent
//     review. The model alone puts the riskiest 18% of changes at High;
//     with the rule floor that is about 26% High and 29% agent reviews in
//     the evaluation, under a ~30% review budget.
//   - reviewed: teams with human reviewers. The calibrated thresholds; High
//     asks for a human review, and an agent review is recommended for High
//     and for strong single signals.
//   - strict: high-stakes code. Medium and High are widened (about 41%
//     High in the evaluation), and every
//     Medium or High change gets an agent review before a human one.
var Presets = map[string]Policy{
	"autonomous": {
		Preset:    "autonomous",
		HighShare: f(0.18),
		Actions: map[risk.Tier]string{
			risk.High:   "Run an agent review of the listed reasons and fix what it finds before shipping.",
			risk.Medium: "Check the listed reasons yourself before shipping.",
			risk.Low:    "No extra review needed.",
		},
		Review: ReviewPolicy{ByTier: true, MinTier: risk.High, MinUncertainty: 2.5,
			Questions: map[string]float64{"weakens_auth": 0.5, "destroys_data": 0.5}, Rules: dangerRules},
	},
	"reviewed": {
		Preset: "reviewed",
		Actions: map[risk.Tier]string{
			risk.High:   "Get a second review of the listed reasons before shipping.",
			risk.Medium: "Review the files named in the reasons before shipping.",
			risk.Low:    "No extra review needed.",
		},
		Review: ReviewPolicy{ByTier: true, MinTier: risk.High,
			Questions: map[string]float64{"weakens_auth": 0.5, "destroys_data": 0.5}, Rules: dangerRules},
	},
	"strict": {
		Preset:      "strict",
		HighShare:   f(0.30),
		MediumShare: f(0.60),
		Actions: map[risk.Tier]string{
			risk.High:   "Run an agent review, then get a human review of the listed reasons before shipping.",
			risk.Medium: "Run an agent review, then review the files named in the reasons before shipping.",
			risk.Low:    "No extra review needed.",
		},
		Review: ReviewPolicy{ByTier: true, MinTier: risk.Medium, MinUncertainty: 2.0,
			Questions: map[string]float64{"weakens_auth": 0.3, "destroys_data": 0.3}, Rules: dangerRules},
	},
}

// ResolvePolicy applies cfg to its preset.
func ResolvePolicy(cfg risk.PolicyConfig) (Policy, error) {
	name := cfg.Preset
	if name == "" {
		name = DefaultPreset
	}
	base, ok := Presets[name]
	if !ok {
		names := make([]string, 0, len(Presets))
		for n := range Presets {
			names = append(names, n)
		}
		sort.Strings(names)
		return Policy{}, fmt.Errorf("unknown preset %q (have %v)", name, names)
	}
	p := base
	p.Actions = cloneMap(base.Actions)
	p.Review.Questions = cloneMap(base.Review.Questions)
	p.Review.Rules = slices.Clone(base.Review.Rules)

	share := func(name string, v *float64) (*float64, error) {
		if v != nil && !(*v >= 0 && *v <= 1) {
			return nil, fmt.Errorf("%s must be between 0 and 1", name)
		}
		return v, nil
	}
	var err error
	if cfg.HighShare != nil {
		if p.HighShare, err = share("high_share", cfg.HighShare); err != nil {
			return Policy{}, err
		}
	}
	if cfg.MediumShare != nil {
		if p.MediumShare, err = share("medium_share", cfg.MediumShare); err != nil {
			return Policy{}, err
		}
	}
	if len(cfg.Thresholds) > 0 {
		th := calibration.Thresholds{Medium: cfg.Thresholds["medium"], High: cfg.Thresholds["high"]}
		for k, v := range cfg.Thresholds {
			if k != "medium" && k != "high" {
				return Policy{}, fmt.Errorf("thresholds: unknown key %q (want medium and high)", k)
			}
			if !(v >= 0 && v <= 1) { // also rejects NaN
				return Policy{}, fmt.Errorf("thresholds.%s must be between 0 and 1", k)
			}
		}
		if _, ok := cfg.Thresholds["high"]; !ok {
			return Policy{}, fmt.Errorf("thresholds: high is required")
		}
		if _, ok := cfg.Thresholds["medium"]; !ok {
			th.Medium = th.High
		}
		if th.Medium > th.High {
			return Policy{}, fmt.Errorf("thresholds: medium %.3f is above high %.3f", th.Medium, th.High)
		}
		p.Thresholds = &th
	}
	if cfg.Overrides != nil {
		p.Overrides = []calibration.Override{}
		for _, o := range cfg.Overrides {
			if _, err := risk.ParseTier(o.Tier); err != nil || o.Question == "" {
				return Policy{}, fmt.Errorf("override %q: needs a question and a tier: %v", o.Question, err)
			}
			if !(o.Min >= 0 && o.Min <= 3) {
				return Policy{}, fmt.Errorf("override %q: min must be between 0 and 1 (yes/no) or 0 and 3 (scores)", o.Question)
			}
			p.Overrides = append(p.Overrides, calibration.Override{Question: o.Question, Min: o.Min, Tier: o.Tier})
		}
	}
	if len(cfg.RuleTiers) > 0 {
		p.RuleTiers = map[string]risk.Tier{}
		for id, s := range cfg.RuleTiers {
			t, err := risk.ParseTier(s)
			if err != nil {
				return Policy{}, fmt.Errorf("rule_tiers %q: %w", id, err)
			}
			p.RuleTiers[id] = t
		}
	}
	for k, v := range cfg.Actions {
		t, err := risk.ParseTier(k)
		if err != nil {
			return Policy{}, fmt.Errorf("actions: %w", err)
		}
		p.Actions[t] = v
	}
	if r := cfg.AgentReview; r != nil {
		if r.MinTier != nil {
			if *r.MinTier == "none" {
				p.Review.ByTier = false
			} else {
				t, err := risk.ParseTier(*r.MinTier)
				if err != nil {
					return Policy{}, fmt.Errorf("agent_review.min_tier: %w", err)
				}
				p.Review.ByTier, p.Review.MinTier = true, t
			}
		}
		if r.MinUncertainty != nil {
			p.Review.MinUncertainty = *r.MinUncertainty
		}
		if r.Questions != nil {
			p.Review.Questions = r.Questions
		}
		if r.Rules != nil {
			p.Review.Rules = r.Rules
		}
	}
	return p, nil
}

// CheckIDs reports rule IDs in rule_tiers and agent_review.rules that no
// rule can set, and sensitive path IDs that clash with built-in ones, so a
// misspelt ID is an error rather than a setting that does nothing.
func (p Policy) CheckIDs(known map[string]bool, project []risk.PathRule) error {
	ids := map[string]bool{}
	for k := range known {
		ids[k] = true
	}
	for _, r := range project {
		if known[r.ID] {
			return fmt.Errorf("sensitive path id %q is also a built-in rule; pick another", r.ID)
		}
		ids[r.ID], ids["config:"+r.ID] = true, true
	}
	var bad []string
	for id := range p.RuleTiers {
		if !ids[id] {
			bad = append(bad, id)
		}
	}
	for _, id := range p.Review.Rules {
		if !ids[id] {
			bad = append(bad, id)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("unknown rule ids %v in rule_tiers or agent_review.rules", bad)
	}
	return nil
}

// Table returns t with the policy's thresholds and overrides.
func (p Policy) Table(t *calibration.Table) (*calibration.Table, error) {
	out := *t
	if p.Overrides != nil {
		out.Overrides = p.Overrides
	}
	switch {
	case p.Thresholds != nil:
		out.Thresholds = *p.Thresholds
	case p.HighShare != nil || p.MediumShare != nil:
		if p.HighShare != nil {
			v, ok := t.ThresholdForShare(*p.HighShare)
			if !ok {
				return t, fmt.Errorf("calibration %s has no risk quantiles for high_share", t.ID)
			}
			out.Thresholds.High = v
		}
		if p.MediumShare != nil {
			v, ok := t.ThresholdForShare(*p.MediumShare)
			if !ok {
				return t, fmt.Errorf("calibration %s has no risk quantiles for medium_share", t.ID)
			}
			out.Thresholds.Medium = v
		}
		out.Thresholds.Medium = min(out.Thresholds.Medium, out.Thresholds.High)
	}
	return &out, nil
}

// AgentReview decides whether to recommend an agent review. hits are rule
// and finding IDs; answers may be nil (rules only). The reason is fixed
// text plus numbers, never an ID or text from the repository or its
// config, because a harness may put it into an agent's prompt unread.
func (p Policy) AgentReview(tier risk.Tier, hits []string, answers map[string]risk.Answer) risk.AgentReview {
	r := p.Review
	if r.ByTier && tier >= r.MinTier {
		return risk.AgentReview{Recommended: true, Reason: fmt.Sprintf("The change is %s risk.", titleTier(tier))}
	}
	for _, id := range r.Rules {
		// Project rules from sensitive_paths hit as config:<id>.
		if slices.Contains(hits, id) || slices.Contains(hits, "config:"+id) {
			text := ruleReviewText[id]
			if text == "" {
				text = "A rule chosen for agent review fired."
			}
			return risk.AgentReview{Recommended: true, Reason: text}
		}
	}
	qs := make([]string, 0, len(r.Questions))
	for q := range r.Questions {
		qs = append(qs, q)
	}
	sort.Strings(qs)
	for _, q := range qs {
		if v := noul(answers[q]); answers[q].Noul != nil && v >= r.Questions[q] {
			text := flagText[q]
			if text == "" {
				text = "Jev flagged a risk"
			}
			return risk.AgentReview{Recommended: true, Reason: fmt.Sprintf("%s (Jev: %.2f).", text, v)}
		}
	}
	if a, ok := answers["uncertainty"]; ok && r.MinUncertainty > 0 && a.Score != nil && *a.Score >= r.MinUncertainty {
		return risk.AgentReview{Recommended: true, Reason: fmt.Sprintf("Jev rates the change weakly verified (%.1f of 3).", *a.Score)}
	}
	return risk.AgentReview{}
}

var ruleReviewText = map[string]string{
	"secret_added":             "The change adds what looks like a secret.",
	"tls_disabled":             "The change turns off TLS certificate checks.",
	"gitignore_hides_code":     "A .gitignore line in the change hides code files.",
	"nested_repo":              "The change adds a nested git repository.",
	"generated_without_source": "The change adds a file that says it is generated, but nothing regenerates it.",
	"generated_hand_edited":    "The agent edited a generated file by hand.",
	"destructive_sql":          "The change adds destructive SQL.",
	"migration":                "The change edits a database migration.",
	"auth":                     "The change edits authentication or authorisation code.",
	"payments":                 "The change edits payment or billing code.",
	"lang:auth_logic_changed":  "The change alters logic inside an access check.",
}

func titleTier(t risk.Tier) string {
	switch t {
	case risk.High:
		return "High"
	case risk.Medium:
		return "Medium"
	}
	return "Low"
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
