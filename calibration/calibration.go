// Package calibration holds the weights and thresholds that turn a
// backend's answers into a tier, one table per backend and model version.
// The tables are data (calibration/*.json), written by the offline
// evaluation and embedded in the binary.
package calibration

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

//go:embed *.json
var files embed.FS

// Table turns features into a 0–1 model risk and a tier.
type Table struct {
	ID     string   `json:"id"`     // e.g. "jev-1.14"
	Models []string `json:"models"` // normalised model versions this table fits
	// Weights per feature. Features are: impact (score/3), uncertainty
	// (score/3), impact_x_uncertainty, flags (the largest noul answer),
	// history (prior fixes to the changed files, 0–1), and size (changed
	// lines on a log scale, 0–1).
	Weights    map[string]float64 `json:"weights"`
	Thresholds Thresholds         `json:"thresholds"`
	// Overrides raise the model tier when one answer alone is strong
	// enough, such as a near-certain yes to "weakens access checks" on a
	// small diff the weighted risk would leave Low. They are set by hand,
	// not fitted: outcome labels are too sparse for these rare cases.
	Overrides []Override `json:"overrides,omitempty"`
	FittedOn  string     `json:"fitted_on,omitempty"`
	Notes     string     `json:"notes,omitempty"`
}

// Thresholds are the model risks at which the model alone supports Medium
// and High.
type Thresholds struct {
	Medium float64 `json:"medium"`
	High   float64 `json:"high"`
}

// Override raises the model tier to Tier when the answer to Question is at
// least Min: the noul probability for yes/no questions, the score for
// score questions.
type Override struct {
	Question string  `json:"question"`
	Min      float64 `json:"min"`
	Tier     string  `json:"tier"` // "medium" or "high"
}

// Risk combines features into a 0–1 risk.
func (t *Table) Risk(features map[string]float64) float64 {
	r := 0.0
	for k, w := range t.Weights {
		r += w * features[k]
	}
	return min(1, max(0, r))
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)
var patchSuffix = regexp.MustCompile(`^(jev-\d+\.\d+)\.\d+$`)

// Normalise maps model IDs from every route to one version:
// "jev-1.14.0" (Amp, TypeSafe), "typesafe/jev-1.14-20261014" (OpenRouter),
// and "typesafe/jev-1.14" all become "jev-1.14".
func Normalise(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "~")
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = dateSuffix.ReplaceAllString(m, "")
	if s := patchSuffix.FindStringSubmatch(m); s != nil {
		m = s[1]
	}
	return m
}

// For returns the table for a model, or the default table and false when
// none matches.
func For(model string) (*Table, bool, error) {
	want := Normalise(model)
	all, err := All()
	if err != nil {
		return nil, false, err
	}
	var def *Table
	for _, t := range all {
		for _, m := range t.Models {
			if m == want {
				return t, true, nil
			}
		}
		if t.ID == "default" {
			def = t
		}
	}
	if def == nil {
		return nil, false, fmt.Errorf("no calibration for %q and no default", model)
	}
	return def, false, nil
}

// ByID returns the embedded table with this ID.
func ByID(id string) (*Table, bool) {
	all, err := All()
	if err != nil {
		return nil, false
	}
	for _, t := range all {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// All returns every embedded table.
func All() ([]*Table, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var out []*Table
	for _, e := range entries {
		b, err := files.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		var t Table
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("calibration/%s: %w", e.Name(), err)
		}
		if t.ID+".json" != path.Base(e.Name()) {
			return nil, fmt.Errorf("calibration/%s: id %q does not match the file name", e.Name(), t.ID)
		}
		out = append(out, &t)
	}
	return out, nil
}
