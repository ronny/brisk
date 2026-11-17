package eval

import (
	"math"
	"math/rand/v2"
	"sort"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/risk"
)

// Scored is a record with brisk's result for one run of answers.
type Scored struct {
	*Record
	Tier      risk.Tier
	ModelRisk float64
	Model     string
	Answered  bool
	ModelMS   float64
}

// Score applies a calibration table to the first run of answers for each
// record, using the engine's own functions.
func Score(recs []Record, runs map[string][]Run, t *calibration.Table, run int) []Scored {
	out := make([]Scored, 0, len(recs))
	for i := range recs {
		r := &recs[i]
		s := Scored{Record: r, Tier: r.Floor}
		if rs := runs[r.RequestID]; len(rs) > run {
			a := rs[run]
			s.Answered, s.Model, s.ModelMS = true, a.Model, a.MS
			s.ModelRisk = t.Risk(assess.Features(a.Answers, r.History, assess.SizeFeature(r.Stats.Additions+r.Stats.Deletions)))
			s.Tier = risk.Max(r.Floor, assess.ModelTier(t, s.ModelRisk))
		}
		out = append(out, s)
	}
	return out
}

// BriskScore orders changes for PR-AUC: tier first, model risk within a tier.
func BriskScore(s Scored) float64 { return float64(s.Tier) + s.ModelRisk*0.999 }

// RulesScore is the path-rules-only baseline: the floor, with the number of
// rule hits breaking ties.
func RulesScore(s Scored) float64 { return float64(s.Floor) + math.Min(0.9, 0.1*float64(len(s.Hits))) }

// SizeScore is the diff-size-only baseline.
func SizeScore(s Scored) float64 { return math.Log1p(float64(s.Stats.Additions + s.Stats.Deletions)) }

// ModelOnlyScore is the model's risk without the rule floor.
func ModelOnlyScore(s Scored) float64 { return s.ModelRisk }

// AveragePrecision is the area under the precision-recall curve, with tied
// scores handled as one threshold.
func AveragePrecision(scores []float64, bad []bool) float64 {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	pos := 0
	for _, b := range bad {
		if b {
			pos++
		}
	}
	if pos == 0 {
		return math.NaN()
	}
	ap, tp, n, prevRecall := 0.0, 0, 0, 0.0
	for i := 0; i < len(idx); {
		j := i
		for j < len(idx) && scores[idx[j]] == scores[idx[i]] {
			if bad[idx[j]] {
				tp++
			}
			n++
			j++
		}
		recall := float64(tp) / float64(pos)
		ap += (recall - prevRecall) * float64(tp) / float64(n)
		prevRecall = recall
		i = j
	}
	return ap
}

// Interval is a point estimate with a 95% bootstrap interval.
type Interval struct {
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

// BootstrapAPDiff estimates AP(a) − AP(b) on the same sample.
func BootstrapAPDiff(a, b []float64, bad []bool, n int, seed uint64) Interval {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	diff := AveragePrecision(a, bad) - AveragePrecision(b, bad)
	var ds []float64
	sa, sb, sbad := make([]float64, len(a)), make([]float64, len(a)), make([]bool, len(a))
	for k := 0; k < n; k++ {
		for i := range a {
			j := rng.IntN(len(a))
			sa[i], sb[i], sbad[i] = a[j], b[j], bad[j]
		}
		d := AveragePrecision(sa, sbad) - AveragePrecision(sb, sbad)
		if !math.IsNaN(d) {
			ds = append(ds, d)
		}
	}
	sort.Float64s(ds)
	if len(ds) == 0 {
		return Interval{Value: diff}
	}
	return Interval{Value: diff, Low: ds[int(0.025*float64(len(ds)))], High: ds[int(0.975*float64(len(ds)-1))]}
}

// Proportion is k of n with a Wilson 95% interval.
type Proportion struct {
	K     int     `json:"k"`
	N     int     `json:"n"`
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

func NewProportion(k, n int) Proportion {
	p := Proportion{K: k, N: n}
	if n == 0 {
		return p
	}
	z := 1.96
	ph := float64(k) / float64(n)
	den := 1 + z*z/float64(n)
	c := (ph + z*z/(2*float64(n))) / den
	h := z * math.Sqrt(ph*(1-ph)/float64(n)+z*z/(4*float64(n)*float64(n))) / den
	p.Value, p.Low, p.High = ph, math.Max(0, c-h), math.Min(1, c+h)
	return p
}

// Summary is the definition-of-done metrics for one set of scored changes.
type Summary struct {
	N           int                   `json:"n"`
	Bad         int                   `json:"bad"`
	Answered    int                   `json:"answered"`
	TierCounts  map[string]int        `json:"tier_counts"`
	BadRate     map[string]Proportion `json:"bad_rate_by_tier"`
	HighRecall  Proportion            `json:"high_recall"`
	HighFlag    Proportion            `json:"high_flag_rate"`
	MedHighRec  Proportion            `json:"medium_or_high_recall"`
	Monotonic   bool                  `json:"monotonic"`
	AP          map[string]float64    `json:"pr_auc"`
	APDiffRules Interval              `json:"pr_auc_brisk_minus_rules"`
	FloorCounts map[string]int        `json:"floor_counts"`
	FloorBad    map[string]Proportion `json:"bad_rate_by_floor"`
}

// Summarise computes the metrics for scored changes.
func Summarise(ss []Scored) Summary {
	sum := Summary{N: len(ss), TierCounts: map[string]int{}, BadRate: map[string]Proportion{}, AP: map[string]float64{},
		FloorCounts: map[string]int{}, FloorBad: map[string]Proportion{}}
	tierBad := map[risk.Tier]int{}
	tierN := map[risk.Tier]int{}
	floorBad := map[risk.Tier]int{}
	floorN := map[risk.Tier]int{}
	var bad []bool
	var b, r, sz, m []float64
	highBad, medHighBad := 0, 0
	for _, s := range ss {
		if s.Bad {
			sum.Bad++
		}
		if s.Answered {
			sum.Answered++
		}
		tierN[s.Tier]++
		floorN[s.Floor]++
		if s.Bad {
			tierBad[s.Tier]++
			floorBad[s.Floor]++
			if s.Tier == risk.High {
				highBad++
			}
			if s.Tier >= risk.Medium {
				medHighBad++
			}
		}
		bad = append(bad, s.Bad)
		b = append(b, BriskScore(s))
		r = append(r, RulesScore(s))
		sz = append(sz, SizeScore(s))
		m = append(m, ModelOnlyScore(s))
	}
	for _, t := range []risk.Tier{risk.Low, risk.Medium, risk.High} {
		sum.TierCounts[t.String()] = tierN[t]
		sum.BadRate[t.String()] = NewProportion(tierBad[t], tierN[t])
		sum.FloorCounts[t.String()] = floorN[t]
		sum.FloorBad[t.String()] = NewProportion(floorBad[t], floorN[t])
	}
	sum.HighRecall = NewProportion(highBad, sum.Bad)
	sum.HighFlag = NewProportion(tierN[risk.High], sum.N)
	sum.MedHighRec = NewProportion(medHighBad, sum.Bad)
	l, me, h := sum.BadRate["low"].Value, sum.BadRate["medium"].Value, sum.BadRate["high"].Value
	sum.Monotonic = tierN[risk.Low] > 0 && tierN[risk.Medium] > 0 && tierN[risk.High] > 0 && l < me && me < h
	sum.AP["brisk"] = AveragePrecision(b, bad)
	sum.AP["rules_only"] = AveragePrecision(r, bad)
	sum.AP["diff_size"] = AveragePrecision(sz, bad)
	sum.AP["model_only"] = AveragePrecision(m, bad)
	sum.AP["base_rate"] = float64(sum.Bad) / math.Max(1, float64(sum.N))
	sum.APDiffRules = BootstrapAPDiff(b, r, bad, 2000, 1)
	return sum
}

// Percentile returns the p-th percentile (0–100) of xs.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(len(s)-1, i))]
}
