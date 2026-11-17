// Package openrouter asks Jev through OpenRouter's System One endpoint,
// which takes TypeSafe's request shape unchanged.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ronny/brisk/risk"
)

// DefaultModel is the Jev version brisk is calibrated for.
const DefaultModel = "typesafe/jev-1.14"

// DefaultURL is OpenRouter's System One endpoint.
const DefaultURL = "https://openrouter.ai/api/v1/systemone"

// Backend calls OpenRouter.
type Backend struct {
	APIKey string
	Model  string
	URL    string
	Client *http.Client
}

var _ risk.Backend = (*Backend)(nil)

// FromEnv returns a backend configured from OPENROUTER_API_KEY, BRISK_MODEL,
// and BRISK_OPENROUTER_URL, or nil when there is no key.
func FromEnv() *Backend {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return nil
	}
	b := &Backend{APIKey: key, Model: os.Getenv("BRISK_MODEL"), URL: os.Getenv("BRISK_OPENROUTER_URL")}
	return b
}

func (b *Backend) Name() string { return "openrouter" }

type request struct {
	Model     string                   `json:"model"`
	State     any                      `json:"state"`
	Questions map[string]risk.Question `json:"questions"`
}

type response struct {
	Model   string                 `json:"model"`
	Answers map[string]risk.Answer `json:"answers"`
	Usage   struct {
		InputTokens int     `json:"input_tokens"`
		Cost        float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Answer implements risk.Backend.
func (b *Backend) Answer(ctx context.Context, req risk.Request) (risk.Response, error) {
	model, url, client := b.Model, b.URL, b.Client
	if model == "" {
		model = DefaultModel
	}
	if url == "" {
		url = DefaultURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(request{Model: model, State: req.State, Questions: req.Questions})
	if err != nil {
		return risk.Response{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return risk.Response{}, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 500 * time.Millisecond):
			}
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return risk.Response{}, err
		}
		hreq.Header.Set("Authorization", "Bearer "+b.APIKey)
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("X-Title", "brisk")
		hreq.Header.Set("HTTP-Referer", "https://github.com/ronny/brisk")
		resp, err := client.Do(hreq)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		var r response
		_ = json.Unmarshal(data, &r)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(data))
			continue
		}
		if resp.StatusCode != http.StatusOK || r.Error != nil {
			msg := snippet(data)
			if r.Error != nil {
				msg = r.Error.Message
			}
			return risk.Response{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}
		for name := range req.Questions {
			if _, ok := r.Answers[name]; !ok {
				return risk.Response{}, fmt.Errorf("no answer for %q", name)
			}
		}
		return risk.Response{Model: r.Model, Answers: r.Answers, Usage: &risk.Usage{InputTokens: r.Usage.InputTokens, CostUSD: r.Usage.Cost}}, nil
	}
	return risk.Response{}, lastErr
}

func snippet(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(bytes.TrimSpace(b))
}
