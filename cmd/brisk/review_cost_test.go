package main

import (
	"context"
	"errors"
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
	v         float64
	reviewErr error
	partial   bool
	calls     int
	reviews   int
}

func (s *stubBackend) Name() string { return "stub" }

func (s *stubBackend) Answer(_ context.Context, req risk.Request) (risk.Response, error) {
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

// The review questions cost a second model call only when the policy wants
// them: by default when a review is recommended. Their cost is added to
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
		{"not recommended: one call", "", 0, nil, false, 1, "not_asked", 1000, false},
		{"recommended: two calls", "", 0.95, nil, false, 2, "tailored", 1100, true},
		{"review call fails", "", 0.95, errors.New("boom"), false, 2, "failed", 1000, true},
		{"always asks", "agent_review:\n  tailor_plan: always\n", 0, nil, false, 2, "tailored", 1100, false},
		{"partial review answers fail", "", 0.95, nil, true, 2, "failed", 1000, true},
		{"never asks", "agent_review:\n  tailor_plan: never\n", 0.95, nil, false, 1, "not_asked", 1000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := migration(t, c.yaml)
			b := &stubBackend{v: c.v, reviewErr: c.reviewErr, partial: c.partial}
			a := p.Answer(ctx, b)
			rp := a.Provenance.ReviewPlan
			if b.calls != c.calls || rp == nil || rp.Status != c.status || a.AgentReview.Recommended != c.wantReviewed {
				t.Fatalf("calls %d review_plan %+v recommended %v; want %d calls, status %s, recommended %v", b.calls, rp, a.AgentReview.Recommended, c.calls, c.status, c.wantReviewed)
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
