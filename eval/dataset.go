package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/ronny/brisk/assess"
	"github.com/ronny/brisk/risk"
)

// Record is one labelled commit with everything brisk computed before the
// model call.
type Record struct {
	Label
	Floor      risk.Tier          `json:"floor"`
	Hits       []string           `json:"hits"`
	Findings   []string           `json:"findings,omitempty"`
	Stats      risk.Stats         `json:"stats"`
	History    float64            `json:"history"`
	RequestID  string             `json:"request_id"`
	StateChars int                `json:"state_chars"`
	PrepareMS  float64            `json:"prepare_ms"`
	Timings    map[string]float64 `json:"timings"`
}

// HostLine is one request for the answering plugin (eval/ampdecide).
type HostLine struct {
	Key       string                   `json:"key"`
	RequestID string                   `json:"request_id"`
	State     any                      `json:"state"`
	Questions map[string]risk.Question `json:"questions"`
}

// Prepare runs the engine up to the model call for every label.
func Prepare(ctx context.Context, eng *assess.Engine, labels []Label, workers int) ([]Record, map[string]HostLine, error) {
	recs := make([]Record, len(labels))
	lines := map[string]HostLine{}
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, workers))
	for i, l := range labels {
		wg.Add(1)
		go func(i int, l Label) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := eng.Prepare(ctx, assess.Input{Base: l.Parent, Head: l.SHA, Task: l.Message})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s %s: %w", l.Repo, l.SHA[:7], err)
				}
				return
			}
			r := Record{Label: l, Floor: p.Floor, History: p.History(), RequestID: p.RequestID, Timings: p.Timings}
			for _, h := range p.Hits {
				r.Hits = append(r.Hits, h.ID)
			}
			for _, f := range p.Findings {
				r.Findings = append(r.Findings, string(f.Kind))
			}
			for _, f := range p.Changes.Files {
				r.Stats.Files++
				r.Stats.Additions += f.Additions
				r.Stats.Deletions += f.Deletions
			}
			for _, v := range p.Timings {
				r.PrepareMS += v
			}
			if p.Request != nil {
				b, _ := json.Marshal(p.Request)
				r.StateChars = len(b)
				lines[p.RequestID] = HostLine{Key: p.RequestID, RequestID: p.RequestID, State: p.Request.State, Questions: p.Request.Questions}
			}
			recs[i] = r
		}(i, l)
	}
	wg.Wait()
	return recs, lines, firstErr
}

// Run is one answer to one request.
type Run struct {
	Key     string                 `json:"key"`
	Request string                 `json:"request_id"`
	Model   string                 `json:"model"`
	Answers map[string]risk.Answer `json:"answers"`
	MS      float64                `json:"ms"`
	Error   string                 `json:"error,omitempty"`
}

// LoadRuns reads answer files written by eval/ampdecide (or the OpenRouter
// runner), grouped by request ID. Errors are skipped.
func LoadRuns(paths ...string) (map[string][]Run, error) {
	out := map[string][]Run{}
	seen := map[string]bool{}
	for _, p := range paths {
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var r Run
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if r.Error != "" || seen[r.Key] {
				continue
			}
			seen[r.Key] = true
			out[r.Request] = append(out[r.Request], r)
		}
		f.Close()
	}
	for _, rs := range out {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].Key < rs[j].Key })
	}
	return out, nil
}

// WriteJSONL writes values one per line.
func WriteJSONL[T any](path string, vs []T) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, v := range vs {
		if err := enc.Encode(v); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadJSONL reads values one per line.
func ReadJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}
