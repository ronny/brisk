package main

// Builds brisk's session record (schema/session.schema.json) from a Claude
// Code transcript: the JSONL file at the hook input's transcript_path.
// Claude Code does not document the transcript format, so entries and
// blocks this does not recognise are skipped. A command it misses makes
// the change look less verified, which errs toward asking a person.

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ronny/brisk/risk"
)

const (
	maxIntent         = 2000
	maxExcerpt        = 1500
	maxExcerptsTotal  = 8000
	maxSessionCommand = 200
)

type ccEntry struct {
	Type             string `json:"type"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type ccBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type ccToolInput struct {
	FilePath        string `json:"file_path"`
	NotebookPath    string `json:"notebook_path"`
	Command         string `json:"command"`
	RunInBackground bool   `json:"run_in_background"`
}

// blocks reads message content, which is a string or an array of blocks.
func blocks(raw json.RawMessage) []ccBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []ccBlock{{Type: "text", Text: s}}
	}
	var bs []ccBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

// notPrompt matches text Claude Code adds to the user's side of the
// transcript: slash command records, command output, and interruptions.
var notPrompt = regexp.MustCompile(`^(?:<(?:command-name|command-message|command-args|local-command-stdout|local-command-stderr|system-reminder)>|\[Request interrupted|Caveat: The messages below)`)

var exitCodeText = regexp.MustCompile(`(?i)\bexit code:?\s*(-?\d+)`)

func blockText(raw json.RawMessage) string {
	var parts []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ccSession reads the transcript, oldest first: the first prompt is the
// intent, later prompts are kept because they hold corrections, and the
// agent's last message is its summary.
func ccSession(in hookInput, root string) *risk.Session {
	s := &risk.Session{Schema: "brisk.session/v1", Harness: "claude-code", SessionID: in.SessionID}
	var prompts []string
	var lastAssistant string
	files := map[string]bool{}
	var fileOrder []string
	type pending struct {
		name  string
		input ccToolInput
	}
	calls := map[string]pending{}

	if f, err := os.Open(in.TranscriptPath); err == nil {
		defer f.Close()
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) == 0 && err != nil {
				break
			}
			var e ccEntry
			if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.IsMeta {
				continue
			}
			switch e.Type {
			case "assistant":
				for _, b := range blocks(e.Message.Content) {
					switch b.Type {
					case "text":
						if t := strings.TrimSpace(b.Text); t != "" {
							lastAssistant = t
						}
					case "tool_use":
						var ti ccToolInput
						_ = json.Unmarshal(b.Input, &ti)
						calls[b.ID] = pending{b.Name, ti}
					}
				}
			case "user":
				bs := blocks(e.Message.Content)
				prompt := !e.IsCompactSummary
				for _, b := range bs {
					if b.Type != "tool_result" {
						continue
					}
					prompt = false
					c, ok := calls[b.ToolUseID]
					if !ok {
						continue
					}
					delete(calls, b.ToolUseID)
					switch c.name {
					case "Edit", "Write", "MultiEdit", "NotebookEdit":
						p := c.input.FilePath
						if p == "" {
							p = c.input.NotebookPath
						}
						if p = relativeTo(p, root); p != "" && !b.IsError && !files[p] {
							files[p] = true
							fileOrder = append(fileOrder, p)
						}
					case "Bash":
						if c.input.Command == "" {
							continue
						}
						cmd := risk.Command{Command: c.input.Command}
						switch {
						case b.IsError:
							code := 1
							if m := exitCodeText.FindStringSubmatch(blockText(b.Content)); m != nil {
								if n, err := strconv.Atoi(m[1]); err == nil && n != 0 {
									code = n
								}
							}
							cmd.ExitCode = &code
						case !c.input.RunInBackground:
							// The Bash tool reports a non-zero exit as an error.
							zero := 0
							cmd.ExitCode = &zero
						}
						s.Commands = append(s.Commands, cmd)
					}
				}
				if t := blockText(e.Message.Content); prompt && t != "" && !notPrompt.MatchString(t) {
					prompts = append(prompts, t)
				}
			}
		}
	}

	if t := strings.TrimSpace(in.LastAssistantMessage); t != "" {
		lastAssistant = t
	}
	if len(prompts) > 0 {
		s.Intent = clip(prompts[0], maxIntent)
	}
	var excerpts []risk.Excerpt
	for _, p := range prompts[min(1, len(prompts)):] {
		excerpts = append(excerpts, risk.Excerpt{Role: "user", Text: clip(p, maxExcerpt)})
	}
	if lastAssistant != "" {
		excerpts = append(excerpts, risk.Excerpt{Role: "assistant", Text: clip(lastAssistant, maxExcerpt)})
	}
	for total(excerpts) > maxExcerptsTotal && len(excerpts) > 1 {
		excerpts = excerpts[1:]
	}
	s.Excerpts = excerpts
	s.FilesTouched = fileOrder
	if n := len(s.Commands); n > maxSessionCommand {
		s.Commands = s.Commands[n-maxSessionCommand:]
	}
	return s
}

func total(es []risk.Excerpt) int {
	n := 0
	for _, e := range es {
		n += len([]rune(e.Text))
	}
	return n
}

// relativeTo makes an absolute path inside root relative to it.
func relativeTo(path, root string) string {
	r := strings.TrimSuffix(root, "/") + "/"
	if rel, ok := strings.CutPrefix(path, r); ok {
		return rel
	}
	return path
}
