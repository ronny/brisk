package calibration_test

import (
	"testing"

	"github.com/ronny/brisk/calibration"
)

func TestNormaliseAndFor(t *testing.T) {
	for _, m := range []string{"jev-1.14.0", "typesafe/jev-1.14-20261014", "typesafe/jev-1.14", "~typesafe/jev-1.14"} {
		if got := calibration.Normalise(m); got != "jev-1.14" {
			t.Errorf("Normalise(%q) = %q, want jev-1.14", m, got)
		}
		if tb, ok, err := calibration.For(m); err != nil || !ok || tb.ID != "jev-1.14" {
			t.Errorf("For(%q) = %v %v %v", m, tb, ok, err)
		}
	}
	tb, ok, err := calibration.For("gpt-6-luna")
	if err != nil || ok || tb.ID != "default" {
		t.Fatalf("unknown model should get the default table: %v %v %v", tb, ok, err)
	}
	if _, err := calibration.All(); err != nil {
		t.Fatal(err)
	}
}
