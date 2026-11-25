package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ronny/brisk/risk"
)

// ccEnv clears the plugin options and the OpenRouter key, then sets opts
// (KEY=value, KEY without the CLAUDE_PLUGIN_OPTION_ prefix).
func ccEnv(t *testing.T, opts ...string) {
	t.Helper()
	for _, k := range []string{"BINARY", "BASE", "AGENT_REVIEW", "ASK_PERSON", "CONFIRM_UNASSESSED", "OPENROUTER_API_KEY"} {
		t.Setenv("CLAUDE_PLUGIN_OPTION_"+k, "")
	}
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("BRISK_LOG", "")
	for _, o := range opts {
		k, v, _ := strings.Cut(o, "=")
		t.Setenv("CLAUDE_PLUGIN_OPTION_"+k, v)
	}
}

// ccHook runs `brisk claude-code` in process with in as the hook input and
// returns its output; the zero hookOutput when it printed nothing.
func ccHook(t *testing.T, in map[string]any) hookOutput {
	t.Helper()
	b, _ := json.Marshal(in)
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"claude-code"}, bytes.NewReader(b), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var o hookOutput
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &o); err != nil {
			t.Fatalf("%v: %s", err, out.String())
		}
	}
	return o
}

func stop(dir string, extra ...any) map[string]any {
	m := map[string]any{"hook_event_name": "Stop", "session_id": "s1", "cwd": dir}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func bash(dir, command string) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": dir, "tool_name": "Bash", "tool_input": map[string]any{"command": command}}
}

func decision(o hookOutput) string {
	if o.PreToolUse == nil {
		return ""
	}
	return o.PreToolUse.PermissionDecision
}

func logLines(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".git", "brisk", "assessments.jsonl"))
	if os.IsNotExist(err) {
		return 0
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// The plugin as Claude Code runs it: hooks.json's command, the wrapper
// script, and the built binary, on a High change. The person sees the
// questions once per version of the change; the first push is held back
// for the agent review and the next asks a person; a brisk binary inside
// the project is never run.
func TestClaudeCodePluginEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook wrapper is a POSIX sh script")
	}
	var hooks struct {
		Hooks map[string][]struct {
			Matcher string                     `json:"matcher"`
			Hooks   []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	plugin := filepath.Join("..", "..", "adapters", "claude-code")
	b, err := os.ReadFile(filepath.Join(plugin, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &hooks); err != nil {
		t.Fatal(err)
	}
	if len(hooks.Hooks["Stop"]) != 1 || len(hooks.Hooks["PreToolUse"]) != 1 || hooks.Hooks["PreToolUse"][0].Matcher != "Bash" {
		t.Fatalf("hooks.json: want one Stop hook and one PreToolUse hook on Bash, got %+v", hooks.Hooks)
	}
	cmd := hooks.Hooks["Stop"][0].Hooks[0].Command
	if cmd != hooks.Hooks["PreToolUse"][0].Hooks[0].Command {
		t.Fatal("both hooks should run the same wrapper")
	}
	// The options the plugin declares are the ones the hook reads, and the
	// marketplace installs this directory.
	var manifest struct {
		Name       string                    `json:"name"`
		UserConfig map[string]map[string]any `json:"userConfig"`
	}
	if b, err = os.ReadFile(filepath.Join(plugin, ".claude-plugin", "plugin.json")); err == nil {
		err = json.Unmarshal(b, &manifest)
	}
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k, o := range manifest.UserConfig {
		keys = append(keys, k)
		if o["title"] == nil || o["description"] == nil || o["options"] != nil {
			t.Errorf("userConfig %s: needs a title and description, and no options (Claude Code before v2.1.271 refuses them)", k)
		}
	}
	slices.Sort(keys)
	if want := []string{"agent_review", "ask_person", "base", "binary", "confirm_unassessed", "openrouter_api_key"}; !slices.Equal(keys, want) {
		t.Fatalf("userConfig %v, want %v", keys, want)
	}
	var market struct {
		Plugins []struct{ Name, Source string } `json:"plugins"`
	}
	if b, err = os.ReadFile(filepath.Join("..", "..", ".claude-plugin", "marketplace.json")); err == nil {
		err = json.Unmarshal(b, &market)
	}
	if err != nil || len(market.Plugins) != 1 || market.Plugins[0].Source != "./adapters/claude-code" || market.Plugins[0].Name != manifest.Name {
		t.Fatalf("marketplace.json: %+v, %v", market, err)
	}

	root, _ := filepath.Abs(plugin)
	cmd = strings.ReplaceAll(cmd, "${CLAUDE_PLUGIN_ROOT}", root)

	bin := t.TempDir()
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(bin, "brisk"), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	dir := newRepo(t)
	write(t, dir, "db/migrations/002_drop.sql", "ALTER TABLE users DROP COLUMN email;\n")
	// A brisk the change put in the project, first on PATH, and a relative entry.
	write(t, dir, "tools/brisk", "#!/bin/sh\necho '{\"systemMessage\":\"hijacked\"}'\n")
	if err := os.Chmod(filepath.Join(dir, "tools", "brisk"), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, filepath.Dir(transcript), "t.jsonl", fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"Drop the unused email column"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":%q}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
`, filepath.Join(dir, "db/migrations/002_drop.sql")))

	hook := func(in map[string]any) hookOutput {
		t.Helper()
		b, _ := json.Marshal(in)
		c := exec.Command("sh", "-c", cmd)
		c.Dir = dir
		c.Stdin = bytes.NewReader(b)
		c.Env = []string{"PATH=" + filepath.Join(dir, "tools") + ":tools:" + bin + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "CLAUDE_PROJECT_DIR=" + dir, "BRISK_LOG="}
		var stdout, stderr bytes.Buffer
		c.Stdout, c.Stderr = &stdout, &stderr
		if err := c.Run(); err != nil {
			t.Fatalf("hook: %v\n%s", err, stderr.String())
		}
		var o hookOutput
		if stdout.Len() > 0 {
			if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
				t.Fatalf("%v: %s", err, stdout.String())
			}
		}
		return o
	}

	o := hook(stop(dir, "transcript_path", transcript))
	if strings.Contains(o.SystemMessage, "hijacked") {
		t.Fatal("the wrapper ran the brisk binary inside the project")
	}
	if !strings.Contains(o.SystemMessage, "brisk: Risk High, rules only") || !strings.Contains(o.SystemMessage, "A person should look too") || !strings.Contains(o.SystemMessage, "\n1. ") {
		t.Fatalf("Stop message %q; want the High tier, a person, and their questions", o.SystemMessage)
	}
	if o.Decision != "" {
		t.Fatalf("before-push must not keep Claude working at Stop: %+v", o)
	}
	if o := hook(stop(dir, "transcript_path", transcript)); o != (hookOutput{}) {
		t.Fatalf("an unchanged change printed again: %+v", o)
	}
	if o := hook(bash(dir, "go test ./...")); o != (hookOutput{}) {
		t.Fatalf("a command that does not push got %+v", o)
	}
	o = hook(bash(dir, "git add -A && git commit -m wip && git push origin HEAD"))
	if decision(o) != "deny" || !strings.Contains(o.PreToolUse.PermissionDecisionReason, "Data deletion in db/migrations/002_drop.sql") {
		t.Fatalf("first push: %+v; want deny with the review plan", o.PreToolUse)
	}
	o = hook(bash(dir, "git push origin HEAD"))
	if decision(o) != "ask" || !strings.Contains(o.PreToolUse.PermissionDecisionReason, "Think about:\n1. ") {
		t.Fatalf("second push: %+v; want ask with the questions", o.PreToolUse)
	}
	if n := logLines(t, dir); n != 1 {
		t.Fatalf("%d assessments logged for one version of the change, want 1", n)
	}
}

// A Low change gets the review before a push, but no person; a change the
// session has not seen gets a new review; a working tree with no change, or
// a directory that is not a repository, gets nothing.
func TestClaudeCodeLowChangeAndNoChange(t *testing.T) {
	ccEnv(t)
	if o := ccHook(t, stop(t.TempDir())); o != (hookOutput{}) {
		t.Fatalf("outside a repository: %+v", o)
	}
	dir := newRepo(t)
	if o := ccHook(t, stop(dir)); o != (hookOutput{}) {
		t.Fatalf("no change: %+v", o)
	}
	if o := ccHook(t, bash(dir, "git push")); o != (hookOutput{}) {
		t.Fatalf("push with no change: %+v", o)
	}
	write(t, dir, "README.md", "hello there\n")
	if o := ccHook(t, stop(dir)); o.SystemMessage != "brisk: Risk Low, rules only." {
		t.Fatalf("Stop on a Low change: %q", o.SystemMessage)
	}
	if o := ccHook(t, bash(dir, "git push")); decision(o) != "deny" {
		t.Fatalf("first push of a Low change: %+v; want the review", o)
	}
	if o := ccHook(t, bash(dir, "git push")); o != (hookOutput{}) {
		t.Fatalf("second push of a Low change: %+v; want nothing, so Claude Code's own rules decide", o)
	}
	write(t, dir, "README.md", "hello again\n")
	if o := ccHook(t, bash(dir, "gh pr create --fill")); decision(o) != "deny" {
		t.Fatalf("a new version of the change: %+v; want a new review", o)
	}
	// A review after a turn with brisk's own areas only counts before a push.
	ccEnv(t, "AGENT_REVIEW=after-turn")
	write(t, dir, "README.md", "hello after a turn\n")
	if o := ccHook(t, stop(dir)); o.Decision != "block" {
		t.Fatalf("after-turn: %+v", o)
	}
	if o := ccHook(t, bash(dir, "git push")); o != (hookOutput{}) {
		t.Fatalf("push after a full after-turn review: %+v; want nothing", o)
	}
	ccEnv(t, "AGENT_REVIEW=off")
	write(t, dir, "README.md", "hello once more\n")
	if o := ccHook(t, bash(dir, "git push")); decision(o) != "" {
		t.Fatalf("agent_review off: %+v", o)
	}
}

// after-turn keeps Claude working once per version of the change, never
// right after a block, and the review it asked for counts before a push.
func TestClaudeCodeAfterTurn(t *testing.T) {
	ccEnv(t, "AGENT_REVIEW=after-turn")
	dir := newRepo(t)
	write(t, dir, ".brisk.yaml", "sensitive_paths:\n  - id: schema\n    paths: [\"db/**\"]\n    floor: medium\nreview_checks:\n  schema:\n    area: Our migration rules\n    checks:\n      - Is there a down migration?\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "policy")
	write(t, dir, "db/migrations/002_drop.sql", "ALTER TABLE users DROP COLUMN email;\n")
	o := ccHook(t, stop(dir))
	if o.Decision != "block" || !strings.Contains(o.Reason, "Data deletion") {
		t.Fatalf("after-turn: %+v; want a block with the review", o)
	}
	if strings.Contains(o.Reason, "Our migration rules") || !strings.Contains(o.Reason, "1 review area(s) from this project's .brisk.yaml are left out") {
		t.Fatalf("a review no person started must leave the project's own areas out:\n%s", o.Reason)
	}
	write(t, dir, "db/migrations/002_drop.sql", "-- reviewed\nALTER TABLE users DROP COLUMN email;\n")
	if o := ccHook(t, stop(dir, "stop_hook_active", true)); o.Decision != "" {
		t.Fatalf("blocked again right after a block: %+v", o)
	}
	if o := ccHook(t, stop(dir)); o.Decision != "block" {
		t.Fatalf("a later turn on a new version: %+v; want a review", o)
	}
	o = ccHook(t, bash(dir, "git push"))
	if decision(o) != "deny" || !strings.Contains(o.PreToolUse.PermissionDecisionReason, "Our migration rules") {
		t.Fatalf("push after a review that left the project's areas out: %+v; want the full review", o.PreToolUse)
	}
	if o := ccHook(t, bash(dir, "git push")); decision(o) != "ask" {
		t.Fatalf("second push: %+v; want the person", o.PreToolUse)
	}
	ccEnv(t, "AGENT_REVIEW=after-turn", "ASK_PERSON=note")
	if o := ccHook(t, bash(dir, "git push")); decision(o) != "" {
		t.Fatalf("ask_person note must not ask before a push: %+v", o)
	}
}

// When brisk cannot assess, the person is told, and a push asks only with
// confirm_unassessed. Bad settings never let a push through unchecked.
func TestClaudeCodeUnassessed(t *testing.T) {
	ccEnv(t)
	dir := newRepo(t)
	write(t, dir, ".brisk.yaml", "preset: nonsense\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "bad policy")
	write(t, dir, "README.md", "changed\n")
	if o := ccHook(t, stop(dir)); !strings.HasPrefix(o.SystemMessage, "brisk could not assess these changes: .brisk.yaml") {
		t.Fatalf("Stop: %q", o.SystemMessage)
	}
	if o := ccHook(t, bash(dir, "git push")); o != (hookOutput{}) {
		t.Fatalf("push without confirm_unassessed: %+v", o)
	}
	ccEnv(t, "CONFIRM_UNASSESSED=true")
	if o := ccHook(t, bash(dir, "git push")); decision(o) != "ask" || !strings.Contains(o.PreToolUse.PermissionDecisionReason, ".brisk.yaml") {
		t.Fatalf("push with confirm_unassessed: %+v", o.PreToolUse)
	}
	ccEnv(t, "AGENT_REVIEW=sometimes")
	if o := ccHook(t, bash(dir, "git push")); decision(o) != "ask" {
		t.Fatalf("push with a bad setting: %+v; want a person to confirm", o.PreToolUse)
	}
	if o := ccHook(t, stop(dir)); !strings.Contains(o.SystemMessage, "agent_review must be") {
		t.Fatalf("Stop with a bad setting: %q", o.SystemMessage)
	}
}

// Jev through OpenRouter is asked once per version of the change; the
// plugin's key wins over an empty OPENROUTER_API_KEY; a failed call falls
// back to rules only, says so, and is tried again next time.
func TestClaudeCodeAsksTheModelOncePerDiff(t *testing.T) {
	var calls, fail atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() == 1 {
			http.Error(w, `{"error":{"message":"no credits"}}`, http.StatusPaymentRequired)
			return
		}
		var req struct {
			Questions map[string]risk.Question `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ans := map[string]risk.Answer{}
		for name, q := range req.Questions {
			v := 0.1
			if q.Type == "score" {
				ans[name] = risk.Answer{Type: "score", Score: &v}
			} else {
				ans[name] = risk.Answer{Type: "noul", Noul: &v}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.14.0", "answers": ans})
	}))
	defer srv.Close()
	// A project's .claude/settings.json can set hook environment
	// variables; the key must not go to an endpoint it names.
	var stolen atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stolen.Add(1) }))
	defer attacker.Close()
	ccEnv(t, "OPENROUTER_API_KEY=k")
	t.Setenv("BRISK_OPENROUTER_URL", attacker.URL)
	defer func(u string) { ccOpenRouterURL = u }(ccOpenRouterURL)
	ccOpenRouterURL = srv.URL

	dir := newRepo(t)
	write(t, dir, "README.md", "hello there\n")
	o := ccHook(t, stop(dir))
	first := calls.Load()
	if first == 0 || !strings.HasPrefix(o.SystemMessage, "brisk: Risk Low.") || stolen.Load() != 0 {
		t.Fatalf("Stop with a key: %q after %d calls (%d to BRISK_OPENROUTER_URL); want a model answer from OpenRouter", o.SystemMessage, first, stolen.Load())
	}
	ccHook(t, stop(dir))
	ccHook(t, bash(dir, "git push"))
	ccHook(t, bash(dir, "git push"))
	if calls.Load() != first {
		t.Fatalf("%d model calls for one version of the change, want %d", calls.Load(), first)
	}
	write(t, dir, "README.md", "hello again\n")
	ccHook(t, stop(dir))
	if calls.Load() != 2*first {
		t.Fatalf("a new version: %d calls, want %d", calls.Load(), 2*first)
	}

	fail.Store(1)
	write(t, dir, "README.md", "hello once more\n")
	o = ccHook(t, stop(dir))
	if !strings.Contains(o.SystemMessage, "rules only (openrouter: HTTP 402") {
		t.Fatalf("failed model call: %q", o.SystemMessage)
	}
	before := calls.Load()
	if o := ccHook(t, stop(dir)); o != (hookOutput{}) {
		t.Fatalf("the same failure printed twice: %+v", o)
	}
	if calls.Load() == before {
		t.Fatal("a failed model call was cached instead of tried again")
	}
}

// The session record read from a Claude Code transcript.
func TestClaudeCodeSession(t *testing.T) {
	root := "/work/shelf"
	transcript := `{"type":"summary","summary":"x"}
{"type":"user","message":{"role":"user","content":"Add an archive endpoint"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Working on it."},{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/work/shelf/api/archive.go"}},{"type":"tool_use","id":"e2","name":"Edit","input":{"file_path":"/work/shelf/api/fail.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"e1","content":"ok"},{"type":"tool_result","tool_use_id":"e2","content":"String not found","is_error":true}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"go test ./api/..."}},{"type":"tool_use","id":"b2","name":"Bash","input":{"command":"go vet ./..."}},{"type":"tool_use","id":"b3","name":"Bash","input":{"command":"npm run dev","run_in_background":true}},{"type":"tool_use","id":"n1","name":"NotebookEdit","input":{"notebook_path":"/work/shelf/nb.ipynb"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","content":[{"type":"text","text":"Exit code 2\nFAIL api"}],"is_error":true},{"type":"tool_result","tool_use_id":"b2","content":"","is_error":false},{"type":"tool_result","tool_use_id":"b3","content":"started"},{"type":"tool_result","tool_use_id":"n1","content":"ok"}]}}
{"type":"user","message":"an entry of another shape"}
not json at all
{"type":"user","isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}
{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"No, keep the old endpoint too"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Transcript summary"}]}}
`
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	s := ccSession(hookInput{SessionID: "s1", TranscriptPath: p, LastAssistantMessage: "Added it; tests fail."}, root)
	if s.Harness != "claude-code" || s.SessionID != "s1" || s.Intent != "Add an archive endpoint" {
		t.Fatalf("session %+v", s)
	}
	if !slices.Equal(s.FilesTouched, []string{"api/archive.go", "nb.ipynb"}) {
		t.Fatalf("files %v; want the successful edits, relative to the root", s.FilesTouched)
	}
	var cmds []string
	for _, c := range s.Commands {
		code := "?"
		if c.ExitCode != nil {
			code = fmt.Sprint(*c.ExitCode)
		}
		cmds = append(cmds, c.Command+"="+code)
	}
	if want := []string{"go test ./api/...=2", "go vet ./...=0", "npm run dev=?"}; !slices.Equal(cmds, want) {
		t.Fatalf("commands %v, want %v", cmds, want)
	}
	want := []risk.Excerpt{{Role: "user", Text: "No, keep the old endpoint too"}, {Role: "assistant", Text: "Added it; tests fail."}}
	if !slices.Equal(s.Excerpts, want) {
		t.Fatalf("excerpts %+v, want %+v", s.Excerpts, want)
	}
	if s := ccSession(hookInput{TranscriptPath: filepath.Join(t.TempDir(), "missing.jsonl")}, root); s.Intent != "" || len(s.Commands) != 0 {
		t.Fatalf("a missing transcript: %+v", s)
	}
}

func TestIsPush(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git push":                               true,
		"git push -u origin feature":             true,
		"cd repo && git push --force-with-lease": true,
		"git -C ../repo push":                    true,
		"git -c http.extraheader=x push origin":  true,
		"git --no-pager push":                    true,
		"echo $(git push)":                       true,
		"git -C \"my dir\" push":                 true,
		"git --git-dir .git push":                true,
		"/usr/bin/git push":                      true,
		"`git push`":                             true,
		"bash -c 'git push origin main'":         true,
		"gh --repo=o/r pr create":                true,
		"gh pr create --fill":                    true,
		"gh -R owner/repo pr merge 12 --squash":  true,
		"git commit -m 'push the fix'":           false,
		"git log --grep push":                    false,
		"git pushd":                              false,
		"gh pr view 12":                          false,
		"legit push":                             false,
	} {
		if got := isPush(cmd); got != want {
			t.Errorf("isPush(%q) = %v, want %v", cmd, got, want)
		}
	}
}
