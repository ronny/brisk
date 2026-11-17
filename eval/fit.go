package eval

import (
	"math"
	"sort"

	"github.com/ronny/brisk/calibration"
	"github.com/ronny/brisk/risk"
)

// FitOptions configures Fit.
type FitOptions struct {
	ID, FittedOn string
	Models       []string
	HighBudget   float64 // share of changes allowed in High, e.g. 0.12
	MediumShare  float64 // share of changes at Medium or above, e.g. 0.40
	// Features limits the fit to these features (for ablations); empty
	// means all.
	Features []string
	// Overrides are kept in the table; the thresholds count the changes
	// they already raise against the budget.
	Overrides []calibration.Override
}

// FeatureNames are every feature a table can weight.
var FeatureNames = []string{"impact", "uncertainty", "impact_x_uncertainty", "flags", "follow_up", "history", "size"}

// Fit chooses weights that maximise PR-AUC of floor+risk on the training
// records, then thresholds that meet the flag budget. It searches a 0.1
// grid on the simplex, which is coarse on purpose: the training set is small.
func Fit(train []Record, runs map[string][]Run, opts FitOptions) *calibration.Table {
	best := &calibration.Table{ID: opts.ID, Models: opts.Models, FittedOn: opts.FittedOn, Overrides: opts.Overrides}
	bestAP := -1.0
	featureNames := FeatureNames
	if len(opts.Features) > 0 {
		featureNames = opts.Features
	}
	var bad []bool
	for _, r := range train {
		bad = append(bad, r.Bad)
	}
	var walk func(i int, left int, w []int)
	walk = func(i int, left int, w []int) {
		if i == len(featureNames)-1 {
			w[i] = left
			t := &calibration.Table{Weights: map[string]float64{}}
			for k, n := range featureNames {
				t.Weights[n] = float64(w[k]) / 10
			}
			ss := Score(train, runs, &calibration.Table{Weights: t.Weights, Thresholds: calibration.Thresholds{Medium: 2, High: 2}}, 0)
			scores := make([]float64, len(ss))
			for j, s := range ss {
				scores[j] = float64(s.Floor) + s.ModelRisk*0.999
			}
			if ap := AveragePrecision(scores, bad); ap > bestAP+1e-9 {
				bestAP = ap
				best.Weights = t.Weights
			}
			return
		}
		for n := 0; n <= left; n++ {
			w[i] = n
			walk(i+1, left-n, w)
		}
	}
	walk(0, 10, make([]int, len(featureNames)))

	// Thresholds from the budget. With unreachable thresholds, each
	// change's tier is its floor raised by any override.
	ss := Score(train, runs, &calibration.Table{Weights: best.Weights, Overrides: opts.Overrides, Thresholds: calibration.Thresholds{Medium: 2, High: 2}}, 0)
	best.Thresholds.High = threshold(ss, risk.High, opts.HighBudget)
	best.Thresholds.Medium = math.Min(best.Thresholds.High, threshold(ss, risk.Medium, opts.MediumShare))
	return best
}

// threshold returns the lowest model-risk cut such that the share of
// changes at tier or above (counting rule floors and overrides) stays
// within share.
func threshold(ss []Scored, tier risk.Tier, share float64) float64 {
	limit := int(math.Floor(share * float64(len(ss))))
	already := 0
	var cand []float64
	for _, s := range ss {
		if s.Tier >= tier {
			already++
		} else if s.Answered {
			cand = append(cand, s.ModelRisk)
		}
	}
	room := limit - already
	if room <= 0 {
		return 1.01 // the floor alone uses the budget; the model never raises to this tier
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(cand)))
	if room >= len(cand) {
		return 0
	}
	// Cut just above the first risk that would exceed the budget, so ties
	// stay out together.
	t := cand[room]
	return math.Round((t+0.0005)*1000) / 1000
}
