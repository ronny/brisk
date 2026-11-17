// Package host lets a harness answer brisk's questions with its own
// decision model. `brisk assess --backend host` prints a risk.HostRequest;
// the harness writes a risk.HostAnswers file; `brisk assess --answers file`
// reads it through this backend, so the same scoring code runs as with any
// other backend.
package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ronny/brisk/risk"
)

// RequestID hashes a request. encoding/json sorts map keys, so the same
// state and questions always give the same ID.
func RequestID(req risk.Request) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Answers is a backend that returns answers a harness already wrote.
type Answers struct {
	File risk.HostAnswers
}

var _ risk.Backend = (*Answers)(nil)

// Load reads a host answers file.
func Load(path string) (*Answers, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a risk.HostAnswers
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if a.Schema != risk.HostAnswersSchema {
		return nil, fmt.Errorf("%s: schema is %q, want %q", path, a.Schema, risk.HostAnswersSchema)
	}
	return &Answers{File: a}, nil
}

func (a *Answers) Name() string { return "host" }

// Answer returns the stored answers if they were written for req.
func (a *Answers) Answer(_ context.Context, req risk.Request) (risk.Response, error) {
	id, err := RequestID(req)
	if err != nil {
		return risk.Response{}, err
	}
	if id != a.File.RequestID {
		return risk.Response{}, fmt.Errorf("answers are for request %s, but this change gives %s; the diff or flags changed since the request was printed", a.File.RequestID, id)
	}
	for name := range req.Questions {
		if _, ok := a.File.Answers[name]; !ok {
			return risk.Response{}, fmt.Errorf("answers file has no answer for %q", name)
		}
	}
	return risk.Response{Model: a.File.Model, Answers: a.File.Answers, Usage: a.File.Usage}, nil
}
