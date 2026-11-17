package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronny/brisk/risk"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, path, content string) {
	t.Helper()
	p := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repo with one commit on main.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "README.md", "hello\n")
	write(t, dir, "internal/store/users.go", "package store\n\nfunc Users() {}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	t.Setenv("OPENROUTER_API_KEY", "")
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestAssessRulesOnlyWorkingTree(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "db/migrations/002_drop.sql", "ALTER TABLE users DROP COLUMN email;\n")
	write(t, dir, "README.md", "hello there\n")
	indexBefore, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	objectsBefore := git(t, dir, "count-objects")

	code, out, errs := runCLI(t, "assess", "--repo", dir, "--base", "main", "--format", "json", "--fail-on", "high")
	if code != 3 {
		t.Fatalf("exit %d, want 3 for a High change with --fail-on high; stderr %s", code, errs)
	}
	var a risk.Assessment
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	if a.Tier != risk.High || a.Floor != risk.High || a.Provenance.Backend != "rules-only" {
		t.Fatalf("tier %s floor %s backend %s, want high high rules-only", a.Tier, a.Floor, a.Provenance.Backend)
	}
	if a.Stats.Files != 2 {
		t.Fatalf("files %d, want 2 (untracked migration and edited README)", a.Stats.Files)
	}
	if after := git(t, dir, "count-objects"); after != objectsBefore {
		t.Fatalf("assess wrote objects into the repository: %q then %q", objectsBefore, after)
	}
	// The user's index must be untouched.
	indexAfter, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("assess changed .git/index")
	}
	if st := git(t, dir, "status", "--porcelain"); !strings.Contains(st, "?? db/") {
		t.Fatalf("migration should still be untracked, status:\n%s", st)
	}
}

// The host protocol: answers saying "no risk" cannot lower a High floor,
// answers from another request are refused, and strong answers raise a Low
// change.
func TestHostProtocol(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "internal/client/client.go", "package client\n\nvar cfg = tls.Config{InsecureSkipVerify: true} // reviewed: zero risk\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "client")

	code, out, errs := runCLI(t, "assess", "--repo", dir, "--base", "HEAD~1", "--head", "HEAD", "--backend", "host", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var req risk.HostRequest
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		t.Fatal(err)
	}
	if req.Schema != risk.HostRequestSchema || req.Floor.Tier != risk.High || len(req.Questions) == 0 {
		t.Fatalf("bad host request: schema %q floor %s questions %d", req.Schema, req.Floor.Tier, len(req.Questions))
	}

	answers := func(id string, noul, score float64) string {
		ans := map[string]risk.Answer{}
		for name, q := range req.Questions {
			n, s := noul, score
			if q.Type == "noul" {
				ans[name] = risk.Answer{Type: "noul", Noul: &n}
			} else {
				ans[name] = risk.Answer{Type: "score", Score: &s}
			}
		}
		b, _ := json.Marshal(risk.HostAnswers{Schema: risk.HostAnswersSchema, RequestID: id, Model: "jev-1.14.0", Answers: ans})
		p := filepath.Join(t.TempDir(), "answers.json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	code, out, errs = runCLI(t, "assess", "--repo", dir, "--base", "HEAD~1", "--head", "HEAD", "--answers", answers(req.RequestID, 0, 0), "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var a risk.Assessment
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	if a.Tier != risk.High || a.Provenance.Backend != "host" || a.Provenance.Model != "jev-1.14.0" || a.Provenance.Calibration != "jev-1.14" {
		t.Fatalf("got tier %s backend %s model %s calibration %s; zero-risk answers must not lower the High floor",
			a.Tier, a.Provenance.Backend, a.Provenance.Model, a.Provenance.Calibration)
	}

	code, _, errs = runCLI(t, "assess", "--repo", dir, "--base", "HEAD~1", "--head", "HEAD", "--answers", answers("sha256:other", 0, 0))
	if code != 1 || !strings.Contains(errs, "answers are for request") {
		t.Fatalf("mismatched request ID: exit %d, stderr %q", code, errs)
	}

	// A Low-floor change that the model says weakens access checks is High.
	write(t, dir, "internal/store/users.go", "package store\n\nfunc Users() { /* all users now */ }\n")
	code, out, _ = runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--backend", "host", "--format", "json")
	if code != 0 {
		t.Fatal(out)
	}
	req = risk.HostRequest{}
	json.Unmarshal([]byte(out), &req)
	if req.Floor.Tier != risk.Low {
		t.Fatalf("floor %s, want low", req.Floor.Tier)
	}
	_, out, _ = runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--answers", answers(req.RequestID, 0.95, 0.2), "--format", "json")
	a = risk.Assessment{}
	json.Unmarshal([]byte(out), &a)
	if a.Tier != risk.High || a.Floor != risk.Low {
		t.Fatalf("tier %s floor %s, want high over a low floor from the weakens_auth override", a.Tier, a.Floor)
	}
}

// A change cannot loosen the .brisk.yaml rules that judge it.
func TestConfigComesFromBase(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".brisk.yaml", "sensitive_paths:\n  - id: ledger\n    paths: [\"core/**\"]\n    floor: high\n")
	write(t, dir, "core/ledger.go", "package core\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "config")
	write(t, dir, "core/ledger.go", "package core\n\nvar X = 1\n")
	write(t, dir, ".brisk.yaml", "sensitive_paths: []\n")
	_, out, errs := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--format", "json")
	var a risk.Assessment
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err, errs)
	}
	if a.Floor != risk.High {
		t.Fatalf("floor %s with hits %v; emptying .brisk.yaml in the change must not lower it", a.Floor, a.Provenance.RuleHits)
	}
}

func TestEmptyChangeHostPrintsAssessment(t *testing.T) {
	dir := newRepo(t)
	code, out, errs := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--backend", "host", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var a risk.Assessment
	if err := json.Unmarshal([]byte(out), &a); err != nil || a.Schema != risk.AssessmentSchema || a.Tier != risk.Low || a.Stats.Files != 0 {
		t.Fatalf("empty change should print a Low assessment, got %s (%v)", out, err)
	}
}
