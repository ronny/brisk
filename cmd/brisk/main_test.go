package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	// answers gives every yes/no question noul and every score question
	// score, except the questions named in only.
	answers := func(id string, noul, score float64, only ...map[string]float64) string {
		ans := map[string]risk.Answer{}
		for name, q := range req.Questions {
			n, s := noul, score
			if len(only) > 0 {
				if v, ok := only[0][name]; ok {
					n, s = v, v
				}
			}
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

	// Reasons name only what raised the tier: weakens_auth fired its
	// override; destroys_data at 0.78 is below its override and is not a
	// reason, though it stays in provenance.answers.
	_, out, _ = runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--format", "json",
		"--answers", answers(req.RequestID, 0, 0.2, map[string]float64{"weakens_auth": 0.95, "destroys_data": 0.78}))
	a = risk.Assessment{}
	json.Unmarshal([]byte(out), &a)
	var ids []string
	for _, r := range a.Reasons {
		ids = append(ids, r.ID)
	}
	if a.Tier != risk.High || !slices.Contains(ids, "weakens_auth") || slices.Contains(ids, "destroys_data") {
		t.Fatalf("tier %s reasons %v; want high with weakens_auth and without destroys_data", a.Tier, ids)
	}
	if _, ok := a.Provenance.Answers["destroys_data"]; !ok {
		t.Fatal("destroys_data should stay in provenance.answers")
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

func assessJSON(t *testing.T, args ...string) risk.Assessment {
	t.Helper()
	code, out, errs := runCLI(t, append([]string{"assess", "--format", "json"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var a risk.Assessment
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err, out)
	}
	return a
}

// Collecting the working tree must not run a clean filter, which a change
// could rewrite to run any command.
func TestWorkingTreeRunsNoCleanFilter(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitattributes", "*.foo filter=review\n")
	write(t, dir, "clean.sh", "#!/bin/sh\ncat\n")
	write(t, dir, "data.foo", "one\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "filter")
	git(t, dir, "config", "filter.review.clean", "sh clean.sh")
	git(t, dir, "config", "filter.review.required", "true")
	write(t, dir, "clean.sh", "#!/bin/sh\nprintf ran > executed-by-assess\ncat\n")
	write(t, dir, "data.foo", "two\n")

	a := assessJSON(t, "--repo", dir, "--base", "HEAD")
	if _, err := os.Stat(filepath.Join(dir, "executed-by-assess")); err == nil {
		t.Fatal("assess ran the change's clean filter")
	}
	if a.Stats.Files != 2 {
		t.Fatalf("files %d, want 2 (clean.sh and data.foo)", a.Stats.Files)
	}
}

// A change's own .gitattributes cannot mark code as binary to hide it,
// in the working tree or between commits.
func TestGitattributesCannotHideCode(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "query.sql", "SELECT * FROM users;\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "query")
	write(t, dir, "query.sql", "DROP TABLE users;\n")
	write(t, dir, ".gitattributes", "*.sql -diff\n")

	if a := assessJSON(t, "--repo", dir, "--base", "HEAD"); a.Floor != risk.High {
		t.Fatalf("working tree: floor %s hits %v, want high from destructive_sql", a.Floor, a.Provenance.RuleHits)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "drop")
	if a := assessJSON(t, "--repo", dir, "--base", "HEAD~1", "--head", "HEAD"); a.Floor != risk.High {
		t.Fatalf("commits: floor %s hits %v, want high from destructive_sql", a.Floor, a.Provenance.RuleHits)
	}
}

// Secrets the rules detect never reach the model: not in added, removed,
// or context lines, not split by clipping, not in a key body, a credential
// file, or the session's intent.
func TestSecretsAreRedactedFromModelState(t *testing.T) {
	aws := "AKIA" + strings.Repeat("Q", 16)
	gh := "ghp_" + strings.Repeat("x", 36)
	keyBody := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7"
	envValue := "hunter2-database-password"
	dir := newRepo(t)
	write(t, dir, "client.py", "old = \""+gh+"\"\nkeep = 1\n")
	keyLines := "-----BEGIN OPENSSH PRIVATE KEY-----\n"
	for i := range 10 {
		keyLines += fmt.Sprintf("BODYLINE%02d%s\n", i, keyBody)
	}
	write(t, dir, "deploy_id_rsa", keyLines+"-----END OPENSSH PRIVATE KEY-----\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "client")
	write(t, dir, "client.py", "pad = \""+strings.Repeat(" ", 290)+aws+"\"\nkeep = 1\n")
	write(t, dir, "key.go", "package x\n\nconst k = `-----BEGIN RSA PRIVATE KEY-----\n"+keyBody+"\n-----END RSA PRIVATE KEY-----`\n")
	write(t, dir, ".env", "DB_PASSWORD="+envValue+"\n")
	// A key on one JSON line, and an edit in the middle of a key file.
	write(t, dir, "sa.json", `{"private_key":"-----BEGIN PRIVATE KEY-----\nJSONBODY`+keyBody+`\n-----END PRIVATE KEY-----\n"}`+"\n")
	write(t, dir, "deploy_id_rsa", strings.Replace(keyLines, "BODYLINE05", "EDITED05", 1)+"-----END OPENSSH PRIVATE KEY-----\n")
	session := filepath.Join(t.TempDir(), "session.json")
	write(t, filepath.Dir(session), "session.json", `{"intent": "use key `+aws+`"}`)

	code, out, errs := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--backend", "host", "--session", session, "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var req risk.HostRequest
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(req.Floor.Provenance.RuleHits, "secret_added") {
		t.Fatalf("hits %v, want secret_added", req.Floor.Provenance.RuleHits)
	}
	state, _ := json.Marshal(req.State)
	for name, leak := range map[string]string{"AWS key": aws, "AWS key prefix": aws[:12], "removed GitHub token": gh, "private key body": keyBody, "one-line JSON key": "JSONBODY", "edited key file": "BODYLINE0", ".env value": envValue} {
		if strings.Contains(string(state), leak) {
			t.Errorf("%s reached the model state", name)
		}
	}
	if !strings.Contains(string(state), "[redacted secret]") || !strings.Contains(string(state), "keep = 1") {
		t.Errorf("state should keep the code around a redacted secret:\n%s", state)
	}
}

// The model state clips long lines, so two changes can give the same state.
// Answers for one must still be refused for the other.
func TestAnswersAreBoundToTheWholeChange(t *testing.T) {
	dir := newRepo(t)
	prefix := "query = " + strings.Repeat(" ", 400)
	write(t, dir, "worker.py", prefix+"\"SELECT 1\"\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "worker")

	hostRequest := func() risk.HostRequest {
		code, out, errs := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--backend", "host", "--format", "json")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errs)
		}
		var req risk.HostRequest
		if err := json.Unmarshal([]byte(out), &req); err != nil {
			t.Fatal(err)
		}
		return req
	}
	write(t, dir, "worker.py", prefix+"\"SELECT 2\"\n")
	first := hostRequest()
	write(t, dir, "worker.py", prefix+"\"SELECT secret FROM users\"\n")
	second := hostRequest()
	s1, _ := json.Marshal(first.State)
	s2, _ := json.Marshal(second.State)
	if string(s1) != string(s2) {
		t.Fatal("test setup: the two states differ, so this does not test clipping")
	}
	if first.RequestID == second.RequestID {
		t.Fatal("changes that clip to the same state got the same request ID")
	}

	ans := map[string]risk.Answer{}
	for name, q := range first.Questions {
		z := 0.0
		if q.Type == "noul" {
			ans[name] = risk.Answer{Type: "noul", Noul: &z}
		} else {
			ans[name] = risk.Answer{Type: "score", Score: &z}
		}
	}
	b, _ := json.Marshal(risk.HostAnswers{Schema: risk.HostAnswersSchema, RequestID: first.RequestID, Model: "jev-1.14.0", Answers: ans})
	path := filepath.Join(t.TempDir(), "answers.json")
	write(t, filepath.Dir(path), "answers.json", string(b))
	code, _, errs := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--answers", path)
	if code != 1 || !strings.Contains(errs, "answers are for request") {
		t.Fatalf("stale answers for a clipped change: exit %d, stderr %q", code, errs)
	}
}

// Renaming code into a generated path does not exempt it from rules,
// language analysis, or the model's diff.
func TestRenameIntoGeneratedPathIsStillAnalysed(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "internal/auth/check.go", "package auth\n\nfunc CanAccess(ok bool) bool {\n\treturn ok\n}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "check")
	git(t, dir, "mv", "internal/auth/check.go", "internal/auth/check_gen.go")
	write(t, dir, "internal/auth/check_gen.go", "package auth\n\nfunc CanAccess(ok bool) bool {\n\treturn true\n}\n")

	a := assessJSON(t, "--repo", dir, "--base", "HEAD")
	hits := a.Provenance.RuleHits
	if a.Floor != risk.High || !slices.Contains(hits, "auth") || !slices.Contains(hits, "lang:auth_logic_changed") {
		t.Fatalf("floor %s hits %v, want high with auth and lang:auth_logic_changed", a.Floor, hits)
	}
	code, out, _ := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--backend", "host", "--format", "json")
	if code != 0 || !strings.Contains(out, "return true") {
		t.Fatalf("the renamed file's diff should reach the model state:\n%s", out)
	}
}

// A generated-looking name is a claim the change makes: a new file under
// one is analysed. An edit to a file that already had such a name is not.
func TestNewFileWithGeneratedNameIsAnalysed(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "internal/auth/api.pb.go", "package auth\n\nvar A = 1\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "pb")
	write(t, dir, "internal/auth/api.pb.go", "package auth\n\nvar A = 2\n")
	if a := assessJSON(t, "--repo", dir, "--base", "HEAD"); a.Floor != risk.Low {
		t.Fatalf("edit to an existing generated file: floor %s hits %v, want low", a.Floor, a.Provenance.RuleHits)
	}
	write(t, dir, "internal/auth/hooks_gen.go", "package auth\n\nfunc init() { bypass = func() bool { return true } }\n")
	a := assessJSON(t, "--repo", dir, "--base", "HEAD")
	if a.Floor < risk.Medium || !slices.Contains(a.Provenance.RuleHits, "auth") {
		t.Fatalf("new hooks_gen.go: floor %s hits %v, want auth at medium or more", a.Floor, a.Provenance.RuleHits)
	}
	_, out, _ := runCLI(t, "assess", "--repo", dir, "--base", "HEAD", "--backend", "host", "--format", "json")
	if !strings.Contains(out, "bypass = func() bool") {
		t.Fatalf("the new file's diff should reach the model state:\n%s", out)
	}
}

// A .gitignore line added by the change cannot hide new code from the
// working-tree assessment. Files a .gitignore already ignored at the base
// stay out.
func TestGitignoreAddedByChangeCannotHideCode(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitignore", "dist/\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "ignore")
	write(t, dir, "dist/bundle.js", "export const x = 1\n")
	bypass := "package auth\n\nfunc init() { bypass = func() bool { return true } }\n"
	write(t, dir, "internal/auth/zz.go", bypass)
	write(t, dir, "internal/auth/.gitignore", "zz.go\n")
	write(t, dir, ".gitignore", "dist/\nnested/\n")
	write(t, dir, "nested/deep/hook.go", bypass)

	a := assessJSON(t, "--repo", dir, "--base", "HEAD")
	hits := a.Provenance.RuleHits
	if a.Floor < risk.Medium || !slices.Contains(hits, "gitignore_hides_code") || !slices.Contains(hits, "auth") {
		t.Fatalf("floor %s hits %v, want gitignore_hides_code and auth", a.Floor, hits)
	}
	if a.Stats.Files != 4 {
		t.Fatalf("files %d, want 4: two .gitignore edits, zz.go, hook.go; not dist/bundle.js", a.Stats.Files)
	}
	if st := git(t, dir, "status", "--porcelain", "--ignored"); !strings.Contains(st, "!! internal/auth/zz.go") {
		t.Fatalf("the real index must be untouched:\n%s", st)
	}
}
