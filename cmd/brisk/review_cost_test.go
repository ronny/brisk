package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/backend/host"
	"github.com/ronny/brisk/context/gitctx"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
)

// stubBackend answers every question with v and counts its calls; review
// calls fail when reviewErr is set, and answer nothing when partial is.
type stubBackend struct {
	mu        sync.Mutex // review and main calls may run at once
	v         float64
	reviewErr error
	partial   bool
	calls     int
	reviews   int
}

func (s *stubBackend) Name() string { return "stub" }

func (s *stubBackend) Answer(_ context.Context, req risk.Request) (risk.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	review := false
	ans := map[string]risk.Answer{}
	for name, q := range req.Questions {
		review = review || len(name) > 7 && name[:7] == "review_"
		v := s.v
		if q.Type == "noul" {
			ans[name] = risk.Answer{Type: "noul", Noul: &v}
		} else {
			v *= 3
			ans[name] = risk.Answer{Type: "score", Score: &v}
		}
	}
	if review {
		s.reviews++
		if s.reviewErr != nil {
			return risk.Response{}, s.reviewErr
		}
		if s.partial {
			ans = map[string]risk.Answer{}
		}
		return risk.Response{Model: "jev-1.14.0", Answers: ans, Usage: &risk.Usage{InputTokens: 100, CostUSD: 0.001}}, nil
	}
	return risk.Response{Model: "jev-1.14.0", Answers: ans, Usage: &risk.Usage{InputTokens: 1000, CostUSD: 0.01}}, nil
}

// The review questions are a second model call: by default always, or with
// tailor_plan needed only when a person is asked. Their cost is added to
// provenance.usage, and a failed review call is told apart from not asking.
func TestReviewCallOnlyWhenWanted(t *testing.T) {
	ctx := context.Background()
	migration := func(t *testing.T, yaml string) *assess.Prepared {
		dir := newRepo(t)
		if yaml != "" {
			write(t, dir, ".brisk.yaml", yaml)
			git(t, dir, "add", "-A")
			git(t, dir, "commit", "-q", "-m", "policy")
		}
		write(t, dir, "db/migrations/003_note.sql", "ALTER TABLE users ADD COLUMN note text;\n")
		repo, err := gitx.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		p, err := (&assess.Engine{Repo: repo, Lang: languageBackends(), Context: &gitctx.Provider{}}).Prepare(ctx, assess.Input{Base: "HEAD"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name         string
		yaml         string
		v            float64 // every answer; 0.95 makes the change High
		reviewErr    error
		partial      bool
		calls        int
		status       string
		tokens       int
		wantReviewed bool
	}{
		{"default asks when no person is needed", "", 0, nil, false, 2, "tailored", 1100, false},
		{"default asks when a person is needed", "", 0.95, nil, false, 2, "tailored", 1100, true},
		{"review call fails", "", 0.95, errors.New("boom"), false, 2, "failed", 1000, true},
		{"partial review answers fail", "", 0.95, nil, true, 2, "failed", 1000, true},
		{"needed: no person, one call", "agent_review:\n  tailor_plan: needed\n", 0, nil, false, 1, "not_asked", 1000, false},
		{"needed: a person, two calls", "agent_review:\n  tailor_plan: needed\n", 0.95, nil, false, 2, "tailored", 1100, true},
		{"recommended is needed", "agent_review:\n  tailor_plan: recommended\n", 0, nil, false, 1, "not_asked", 1000, false},
		{"never asks", "agent_review:\n  tailor_plan: never\n", 0.95, nil, false, 1, "not_asked", 1000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := migration(t, c.yaml)
			b := &stubBackend{v: c.v, reviewErr: c.reviewErr, partial: c.partial}
			a := p.Answer(ctx, b)
			rp := a.Provenance.ReviewPlan
			if b.calls != c.calls || rp == nil || rp.Status != c.status || a.HumanCheck.Needed != c.wantReviewed {
				t.Fatalf("calls %d review_plan %+v needed %v; want %d calls, status %s, needed %v", b.calls, rp, a.HumanCheck.Needed, c.calls, c.status, c.wantReviewed)
			}
			if a.Provenance.Usage == nil || a.Provenance.Usage.InputTokens != c.tokens {
				t.Fatalf("usage %+v, want %d input tokens", a.Provenance.Usage, c.tokens)
			}
			if a.Provenance.Degraded != "" || (c.status == "failed") != (rp.Error != "") {
				t.Fatalf("degraded %q error %q", a.Provenance.Degraded, rp.Error)
			}
		})
	}
}

// With nothing asked (tailor_plan: never), a harness's review answers and
// review_usage are ignored.
func TestHostReviewIgnoredWhenNotAsked(t *testing.T) {
	h := &host.Answers{File: risk.HostAnswers{
		ReviewAnswers: map[string]risk.Answer{"review_x": {Type: "score"}},
		ReviewUsage:   &risk.Usage{InputTokens: 100},
	}}
	if ans, errText, usage := h.ReviewResult(); ans != nil || errText != "" || usage != nil {
		t.Fatalf("got %v %q %v, want nothing", ans, errText, usage)
	}
}

// prepare commits yaml as the base .brisk.yaml, writes files as the change,
// and prepares it.
func prepare(t *testing.T, yaml string, files map[string]string) *assess.Prepared {
	t.Helper()
	ctx := context.Background()
	dir := newRepo(t)
	if yaml != "" {
		write(t, dir, ".brisk.yaml", yaml)
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", "policy")
	}
	for path, content := range files {
		write(t, dir, path, content)
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := (&assess.Engine{Repo: repo, Lang: languageBackends(), Context: &gitctx.Provider{}}).Prepare(ctx, assess.Input{Base: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func planIDs(a risk.Assessment) []string {
	var s []string
	for _, x := range a.AgentReview.Plan {
		s = append(s, x.ID)
	}
	return s
}

// An area Jev did not score, such as the generic one a fired project
// question adds, ranks after the areas Jev scored.
func TestUnscoredAreaRanksAfterScored(t *testing.T) {
	p := prepare(t, `content_rules:
  - id: sharing
    paths: ["docs/**"]
    pattern: 'Visible'
    floor: medium
    reason: Sharing rule.
review_checks:
  sharing:
    area: Document sharing
    checks: [Confirm drafts stay hidden.]
questions:
  - id: shares_drafts
    text: Does this change make draft documents visible to others?
    raise_to: medium
`, map[string]string{"docs/share.go": "package docs\n\nvar Visible = true\n"})
	a := p.Answer(context.Background(), &stubBackend{v: 0.95})
	ids := planIDs(a)
	i, j := slices.Index(ids, "config_sharing"), slices.Index(ids, "project_rule")
	if a.Provenance.ReviewPlan.Status != "tailored" || i < 0 || j < 0 || i > j {
		t.Fatalf("plan %v status %s; want the scored config_sharing before the unscored project_rule", ids, a.Provenance.ReviewPlan.Status)
	}
}

// A person asked for by an answer alone, below every override, still gets
// the area for that answer, and it is what they are asked to consider.
func TestAnswerRecommendedReviewHasAPlan(t *testing.T) {
	p := prepare(t, "", map[string]string{"internal/store/users.go": "package store\n\nfunc Users() { /* all users */ }\n"})
	a := p.Answer(context.Background(), &stubBackend{v: 0.6})
	if h := a.HumanCheck; !h.Needed || h.Why != "answer" || !slices.Contains(planIDs(a), "access_checks") || !slices.ContainsFunc(h.Consider, func(c risk.Consider) bool { return c.ID == "access_checks" }) {
		t.Fatalf("human_check %+v plan %v; want access_checks", a.HumanCheck, planIDs(a))
	}
	for _, x := range a.AgentReview.Plan {
		if x.Required {
			t.Fatalf("area %s from a sub-override answer must not be required", x.ID)
		}
	}
}

// Within required areas, an unscored one from a High model override leads
// a required area Jev scored.
func TestOverrideAreaLeadsRequired(t *testing.T) {
	p := prepare(t, `content_rules:
  - id: sharing
    paths: ["docs/**"]
    pattern: 'Visible'
    floor: high
    reason: Sharing rule.
review_checks:
  sharing:
    area: Document sharing
    checks: [Confirm drafts stay hidden.]
`, map[string]string{"docs/share.go": "package docs\n\nvar Visible = true\n"})
	a := p.Answer(context.Background(), &stubBackend{v: 0.95})
	ids := planIDs(a)
	i, j := slices.Index(ids, "access_checks"), slices.Index(ids, "config_sharing")
	if a.Provenance.ReviewPlan.Status != "tailored" || i < 0 || j < 0 || i > j {
		t.Fatalf("plan %v; want access_checks (High override) before the scored config_sharing", ids)
	}
}

// human_check.consider gives a person at most two questions, from the top
// of the review plan: brisk's question for a catalog area, the team's first
// check for its own area, and a general question when the plan is empty.
// Nothing when no person is needed.
func TestHumanCheckConsider(t *testing.T) {
	ctx := context.Background()
	t.Run("plan order, team text, at most two", func(t *testing.T) {
		p := prepare(t, `content_rules:
  - id: sharing
    paths: ["docs/**"]
    pattern: 'Visible'
    floor: high
    reason: Sharing rule.
review_checks:
  sharing:
    area: Document sharing
    checks: [Confirm drafts stay hidden from shared links., Second check.]
`, map[string]string{"docs/share.go": "package docs\n\nvar Visible = true\n", "db/migrations/004.sql": "DROP TABLE notes;\n"})
		a := p.Answer(ctx, &stubBackend{v: 0.95})
		h := a.HumanCheck
		if !h.Needed || len(h.Consider) != 2 || len(a.AgentReview.Plan) < 3 {
			t.Fatalf("human_check %+v plan %v; want two questions from a longer plan", h, planIDs(a))
		}
		for i, c := range h.Consider {
			if c.ID != a.AgentReview.Plan[i].ID {
				t.Fatalf("consider %d is %s, want plan order %v", i, c.ID, planIDs(a))
			}
			want := map[string]string{"config_sharing": "Confirm drafts stay hidden from shared links.", "data_loss": "Is it safe to lose the data this deletes, and is there a way back if not?"}[c.ID]
			if want != "" && c.Text != want {
				t.Fatalf("consider %s text %q, want %q", c.ID, c.Text, want)
			}
		}
	})
	t.Run("not needed: nothing to consider", func(t *testing.T) {
		p := prepare(t, "", map[string]string{"db/migrations/003_note.sql": "ALTER TABLE users ADD COLUMN note text;\n"})
		a := p.Answer(ctx, &stubBackend{v: 0.4}) // relevance 1.2 keeps the area
		if h := a.HumanCheck; h.Needed || h.Why != "" || h.Reason != "" || h.Consider == nil || len(h.Consider) != 0 || len(a.AgentReview.Plan) == 0 {
			t.Fatalf("human_check %+v plan %v; want not needed with an empty list and a plan for the agent", h, planIDs(a))
		}
	})
	t.Run("empty plan: the general question", func(t *testing.T) {
		p := prepare(t, "", map[string]string{"calc/add.go": "package calc\n\nfunc Add(a, b int) int { return a + b }\n"})
		three, zero := 2.9, 0.0
		ans := map[string]risk.Answer{}
		for name, q := range p.Request.Questions {
			if q.Type == "score" {
				ans[name] = risk.Answer{Type: "score", Score: &zero}
			} else {
				ans[name] = risk.Answer{Type: "noul", Noul: &zero}
			}
		}
		ans["impact"] = risk.Answer{Type: "score", Score: &three}
		a := p.Finish("stub", &risk.Response{Model: "jev-1.14.0", Answers: ans}, "")
		h := a.HumanCheck
		if a.Tier != risk.High || len(a.AgentReview.Plan) != 0 || !h.Needed || len(h.Consider) != 1 || h.Consider[0].ID != "change_scope" {
			t.Fatalf("tier %s plan %v human_check %+v; want High, empty plan, change_scope", a.Tier, planIDs(a), h)
		}
	})
}
