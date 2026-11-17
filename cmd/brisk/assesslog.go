package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
)

// LogLine is one live assessment in the local log, so real use can be
// judged later: whether brisk asked for a person, what it asked them to
// consider, and enough to find the change and its later fixes again.
type LogLine struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id,omitempty"`
	// Files are the changed paths, to match the change against later fixes.
	Files      []string        `json:"files"`
	Assessment risk.Assessment `json:"assessment"`
}

// logPath is where assessments are logged: BRISK_LOG when set ("off"
// turns logging off), otherwise brisk/assessments.jsonl in the
// repository's git directory, which is never committed.
func logPath(ctx context.Context, repo *gitx.Repo) string {
	switch v := os.Getenv("BRISK_LOG"); v {
	case "off":
		return ""
	case "":
		out, err := repo.Run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return ""
		}
		return filepath.Join(strings.TrimSpace(string(out)), "brisk", "assessments.jsonl")
	default:
		return v
	}
}

// logAssessment appends a to the local log. It never fails the
// assessment: an error comes back for a warning only.
func logAssessment(path string, p *assess.Prepared, a risk.Assessment) error {
	if path == "" {
		return nil
	}
	l := LogLine{Time: time.Now().UTC(), RequestID: p.RequestID, Files: []string{}, Assessment: a}
	if p.Changes != nil {
		for _, f := range p.Changes.Files {
			l.Files = append(l.Files, f.Path)
		}
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
