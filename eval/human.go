package eval

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
	"github.com/ronny/brisk/rules"
)

// Split returns each repo's oldest share of records as train and the rest
// as test, leaving out censored and unprepared records: the split score
// uses.
func Split(recs []Record, share float64) (train, test []Record) {
	byRepo := map[string][]Record{}
	for _, r := range recs {
		if !r.Censored && r.RequestID != "" {
			byRepo[r.Repo] = append(byRepo[r.Repo], r)
		}
	}
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		rs := byRepo[repo]
		sort.Slice(rs, func(i, j int) bool { return rs[i].When.Before(rs[j].When) })
		cut := int(share * float64(len(rs)))
		train = append(train, rs[:cut]...)
		test = append(test, rs[cut:]...)
	}
	return train, test
}

// FixFiles returns the code files the fix commits changed: the files a
// later fix of this change touched, leaving out docs, tests and generated
// files, like the SZZ labels.
func FixFiles(ctx context.Context, repo *gitx.Repo, fixes []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, sha := range fixes {
		b, err := repo.Run(ctx, "diff-tree", "--no-commit-id", "--name-only", "-r", "--no-renames", sha)
		if err != nil {
			return nil, err
		}
		for _, f := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if f != "" && !rules.IsDoc(f) && !rules.IsTest(f) && !rules.IsGenerated(f) {
				out[f] = true
			}
		}
	}
	return out, nil
}

func cleanPath(p string) string {
	p = path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "./"))
	return strings.TrimPrefix(p, "/")
}

// PromptHit reports whether one of human_check's questions points at a
// file the later fix changed.
func PromptHit(cs []risk.Consider, fix map[string]bool) bool {
	for _, c := range cs {
		for _, e := range c.Evidence {
			if fix[cleanPath(e.Path)] {
				return true
			}
		}
	}
	return false
}
