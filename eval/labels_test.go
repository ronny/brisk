package eval_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ronny/brisk/eval"
	"github.com/ronny/brisk/internal/gitx"
)

func TestLabels(t *testing.T) {
	dir := t.TempDir()
	day0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	commit := func(day int, msg string, files map[string]string) string {
		for p, s := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
			if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		date := day0.Add(time.Duration(day) * 24 * time.Hour).Format(time.RFC3339)
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}, {"rev-parse", "HEAD"}} {
			cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %v %s", args, err, out)
			}
			if args[0] == "rev-parse" {
				return string(out[:40])
			}
		}
		return ""
	}
	exec.Command("git", "init", "-q", "-b", "main", dir).Run()
	commit(0, "base", map[string]string{"a.go": "package a\n\nfunc A() int { return 1 }\n", "b.go": "package a\n\nfunc B() int { return 1 }\n"})
	quick := commit(1, "a: return two", map[string]string{"a.go": "package a\n\nfunc A() int { return 2 }\n"})
	late := commit(2, "b: return two", map[string]string{"b.go": "package a\n\nfunc B() int { return 2 }\n"})
	commit(5, "a: fix A returning the wrong value", map[string]string{"a.go": "package a\n\nfunc A() int { return 3 }\n"})
	commit(30, "b: fix B returning the wrong value", map[string]string{"b.go": "package a\n\nfunc B() int { return 3 }\n"})
	reverted := commit(31, "c: add C", map[string]string{"c.go": "package a\n\nfunc C() {}\n"})
	commit(32, `Revert "c: add C"`, map[string]string{"c.go": ""})
	// A fix that also removes a SQL comment: the removed "-- note" line
	// shows as "--- note" in the diff and must not end the file's hunks.
	commit(20, "q: add", map[string]string{"q.sql": "-- note\nSELECT 1;\nSELECT count(*) FROM t;\n"})
	sqlBad := commit(40, "q: count", map[string]string{"q.sql": "-- note\nSELECT 1;\nSELECT count(*) FROM t2;\n"})
	commit(42, "q: fix the count", map[string]string{"q.sql": "SELECT 1;\nSELECT count(*) FROM t2 WHERE live;\n"})
	// Revert by subject, then a re-land with the same subject.
	landed := commit(44, "e: add E", map[string]string{"e.go": "package a\n\nfunc E() {}\n"})
	commit(45, `Revert "e: add E"`, map[string]string{"e.go": ""})
	relanded := commit(46, "e: add E", map[string]string{"e.go": "package a\n\nfunc E() { /* v2 */ }\n"})
	commit(60, "d: add D", map[string]string{"d.go": "package a\n"})

	repo, err := gitx.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := eval.Labels(context.Background(), repo, eval.LabelOptions{Repo: "t", Since: day0})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]eval.Label{}
	for _, l := range ls {
		by[l.SHA] = l
	}
	if l := by[quick]; !l.Bad || len(l.FixedBy) != 1 {
		t.Errorf("a change fixed 4 days later should be bad: %+v", l)
	}
	if l := by[late]; l.Bad {
		t.Errorf("a change fixed 28 days later should not be bad: %+v", l)
	}
	if l := by[reverted]; !l.Bad || !l.Reverted {
		t.Errorf("a reverted change should be bad: %+v", l)
	}
	if l := by[sqlBad]; !l.Bad {
		t.Errorf("SZZ should blame the SQL change despite the removed comment: %+v", l)
	}
	if !by[landed].Reverted || by[relanded].Reverted {
		t.Errorf("the revert should mark the first landing, not the re-land: %v %v", by[landed].Reverted, by[relanded].Reverted)
	}
	for _, l := range ls {
		if l.Subject == "d: add D" && !l.Censored {
			t.Errorf("the newest change should be censored: %+v", l)
		}
	}
}
