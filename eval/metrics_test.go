package eval_test

import (
	"math"
	"testing"

	"github.com/ronny/brisk/eval"
)

func TestAveragePrecision(t *testing.T) {
	// Ranking bad, good, bad, good: precision 1 at recall 0.5, 2/3 at recall 1.
	got := eval.AveragePrecision([]float64{4, 3, 2, 1}, []bool{true, false, true, false})
	if want := 0.5*1 + 0.5*2.0/3; math.Abs(got-want) > 1e-9 {
		t.Fatalf("AP %v, want %v", got, want)
	}
	// All tied: one threshold, precision equals the base rate.
	if got := eval.AveragePrecision([]float64{1, 1, 1, 1}, []bool{true, false, false, false}); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("tied AP %v, want 0.25", got)
	}
	p := eval.NewProportion(3, 10)
	if p.Value != 0.3 || p.Low > 0.11 || p.Low < 0.1 || p.High < 0.6 || p.High > 0.61 {
		t.Fatalf("Wilson interval %+v, want about [0.108, 0.603]", p)
	}
}
