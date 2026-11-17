package eval_test

import (
	"testing"
	"time"

	"github.com/ronny/brisk/eval"
	"github.com/ronny/brisk/risk"
)

// A prompt hits when one of its evidence paths is a file the later fix
// changed, however the path is written.
func TestPromptHit(t *testing.T) {
	fix := map[string]bool{"internal/web/documents.go": true}
	hit := []risk.Consider{{ID: "a", Evidence: []risk.Evidence{{Path: "internal/web/users.go"}}}, {ID: "b", Evidence: []risk.Evidence{{Path: "./internal/web/documents.go", StartLine: 3}}}}
	if !eval.PromptHit(hit, fix) || eval.PromptHit(hit[:1], fix) || eval.PromptHit(nil, fix) {
		t.Fatal("prompt hit must follow the evidence paths")
	}
}

// Split keeps each repo's oldest share for training, by time, not by
// input order, and leaves out censored and unprepared records.
func TestSplitIsPerRepoByTime(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	rec := func(repo, sha string, d int, censored bool) eval.Record {
		return eval.Record{Label: eval.Label{Repo: repo, SHA: sha, When: day(d), Censored: censored}, RequestID: "r" + sha}
	}
	recs := []eval.Record{
		rec("a", "a3", 3, false), rec("a", "a1", 1, false), rec("a", "a2", 2, false), rec("a", "a4", 4, false), rec("a", "a5", 5, true),
		rec("b", "b2", 2, false), rec("b", "b1", 1, false),
		{Label: eval.Label{Repo: "b", SHA: "b0", When: day(9)}},
	}
	train, test := eval.Split(recs, 0.5)
	sha := func(rs []eval.Record) (s []string) {
		for _, r := range rs {
			s = append(s, r.SHA)
		}
		return s
	}
	if got := sha(train); len(got) != 3 || got[0] != "a1" || got[1] != "a2" || got[2] != "b1" {
		t.Fatalf("train %v, want [a1 a2 b1]", got)
	}
	if got := sha(test); len(got) != 3 || got[0] != "a3" || got[1] != "a4" || got[2] != "b2" {
		t.Fatalf("test %v, want [a3 a4 b2]", got)
	}
}
