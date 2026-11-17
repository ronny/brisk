// Package risk holds the types and interfaces shared by every part of brisk:
// the assessment it prints, the session record a harness supplies, the typed
// questions a classifier backend answers, and the interfaces for backends,
// context providers, and language backends.
//
// The package has no dependencies outside the standard library, so language
// backends and adapters can import it without importing the engine.
package risk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tier is the overall risk of a change.
type Tier int

const (
	Low Tier = iota
	Medium
	High
)

func (t Tier) String() string {
	switch t {
	case Low:
		return "low"
	case Medium:
		return "medium"
	case High:
		return "high"
	}
	return fmt.Sprintf("tier(%d)", int(t))
}

// ParseTier reads "low", "medium", or "high", in any case.
func ParseTier(s string) (Tier, error) {
	switch strings.ToLower(s) {
	case "low":
		return Low, nil
	case "medium":
		return Medium, nil
	case "high":
		return High, nil
	}
	return Low, fmt.Errorf("unknown tier %q", s)
}

func (t Tier) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

func (t *Tier) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParseTier(s)
	*t = v
	return err
}

// Max returns the higher of two tiers.
func Max(a, b Tier) Tier {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Changes

// FileStatus says how a file changed between base and head.
type FileStatus string

const (
	Added    FileStatus = "added"
	Modified FileStatus = "modified"
	Deleted  FileStatus = "deleted"
	Renamed  FileStatus = "renamed"
)

// Hunk is one changed region, in the line numbers of the old and new file.
// A pure insertion has OldLines 0; a pure deletion has NewLines 0.
type Hunk struct {
	OldStart int `json:"old_start"`
	OldLines int `json:"old_lines"`
	NewStart int `json:"new_start"`
	NewLines int `json:"new_lines"`
	// Lines holds the hunk body with its " ", "+", and "-" prefixes.
	Lines []string `json:"-"`
}

// FileChange is one changed file. Old is empty for added files and New is
// empty for deleted files. Binary files have Binary set and no content.
type FileChange struct {
	Path      string     `json:"path"`
	OldPath   string     `json:"old_path,omitempty"`
	Status    FileStatus `json:"status"`
	Binary    bool       `json:"binary,omitempty"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Hunks     []Hunk     `json:"-"`
	Old       []byte     `json:"-"`
	New       []byte     `json:"-"`
	// HandEdited is set when the session shows the agent edited a file
	// that was generated before the change; it is then analysed as code.
	HandEdited bool `json:"hand_edited,omitempty"`
}

// ChangeSet is the whole change being assessed.
type ChangeSet struct {
	RepoRoot string       `json:"-"`
	Base     string       `json:"base"`           // commit the change is measured from
	Head     string       `json:"head,omitempty"` // commit, or "" for the working tree
	Files    []FileChange `json:"files"`
	DiffHash string       `json:"diff_hash"`
	// IgnoredByChange lists untracked code files that only a .gitignore
	// line added by the change ignores. They are assessed as added files.
	IgnoredByChange []string `json:"ignored_by_change,omitempty"`
	// Gitlinks lists submodules or nested repositories the change adds.
	// In the working tree, a nested repository's code files are assessed
	// as added files.
	Gitlinks []string `json:"gitlinks,omitempty"`
}

// ---------------------------------------------------------------------------
// Session record (schema/session.schema.json)

// Session is what a harness knows about the session that produced the
// change. Every field is optional.
type Session struct {
	Schema       string    `json:"schema,omitempty"` // "brisk.session/v1"
	Harness      string    `json:"harness,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	Intent       string    `json:"intent,omitempty"`
	FilesTouched []string  `json:"files_touched,omitempty"`
	Commands     []Command `json:"commands,omitempty"`
	Excerpts     []Excerpt `json:"excerpts,omitempty"`
}

// Command is a shell command the agent ran. ExitCode is nil when unknown.
type Command struct {
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Excerpt is a piece of the transcript. Role is "user" or "assistant".
type Excerpt struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// Assessment (schema/assessment.schema.json)

const AssessmentSchema = "brisk.assessment/v1"

// Assessment is brisk's output.
type Assessment struct {
	Schema      string     `json:"schema"`
	Tier        Tier       `json:"tier"`
	Floor       Tier       `json:"floor"`       // minimum tier set by rules
	Impact      float64    `json:"impact"`      // 0–3
	Uncertainty float64    `json:"uncertainty"` // 0–3
	Action      string     `json:"action"`
	Reasons     []Reason   `json:"reasons"` // at most 3, strongest first
	Stats       Stats      `json:"stats"`
	Provenance  Provenance `json:"provenance"`
}

// Stats summarises the size of the change.
type Stats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Reason explains one thing that drives the tier.
type Reason struct {
	Text     string     `json:"text"`
	Source   string     `json:"source"` // "rule", "language", "question", "context", "session"
	ID       string     `json:"id"`     // rule ID, finding kind, or question name
	Tier     Tier       `json:"tier"`   // the tier this reason alone supports
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Evidence points at the lines a reason is about.
type Evidence struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
}

// Provenance records what produced the assessment.
type Provenance struct {
	BriskVersion string             `json:"brisk_version"`
	Backend      string             `json:"backend"`               // "openrouter", "host", "rules-only"
	Model        string             `json:"model,omitempty"`       // exact version that answered
	Calibration  string             `json:"calibration,omitempty"` // calibration table used
	Degraded     string             `json:"degraded,omitempty"`    // why a backend fell back to rules only
	Base         string             `json:"base"`
	Head         string             `json:"head,omitempty"`
	DiffHash     string             `json:"diff_hash"`
	RuleHits     []string           `json:"rule_hits"`
	Findings     []Finding          `json:"findings,omitempty"`
	Answers      map[string]Answer  `json:"answers,omitempty"`
	ModelRisk    *float64           `json:"model_risk,omitempty"` // combined 0–1 risk from the answers
	Usage        *Usage             `json:"usage,omitempty"`
	DurationMS   int64              `json:"duration_ms"`
	Timings      map[string]float64 `json:"timings_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// Typed questions. The shapes match TypeSafe's System One API, OpenRouter's
// /api/v1/systemone, and Amp's amp.ai.decide, so a request can be sent to
// any of them unchanged.

// Question is a noul (yes/no) or score (ordered levels) question.
type Question struct {
	Type         string `json:"type"` // "noul" or "score"
	Instructions any    `json:"instructions"`
	// Criteria is {"true": ..., "false": ...} for noul and an array of
	// level descriptions, lowest first, for score.
	Criteria any `json:"criteria,omitempty"`
}

// Request is one set of questions about one state.
type Request struct {
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one typed answer. Noul answers set Noul; score answers set
// Score, Probabilities, and Confidence.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Usage is token use and cost, when the backend reports them.
type Usage struct {
	InputTokens int     `json:"input_tokens"`
	CostUSD     float64 `json:"cost_usd,omitempty"`
}

// Response holds the answers to a Request.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   *Usage            `json:"usage,omitempty"`
}

// Backend answers typed risk questions.
type Backend interface {
	// Name is recorded in provenance: "openrouter", "host", or "rules-only".
	Name() string
	// Answer returns the answers to every question in req. Backends that
	// cannot answer (rules-only) return ErrNoModel.
	Answer(ctx context.Context, req Request) (Response, error)
}

// ErrNoModel is returned by backends that do not answer questions.
var ErrNoModel = fmt.Errorf("backend does not answer questions")

// ---------------------------------------------------------------------------
// Host protocol (schema/host-request.schema.json, schema/host-answers.schema.json)

const (
	HostRequestSchema = "brisk.host-request/v1"
	HostAnswersSchema = "brisk.host-answers/v1"
)

// HostRequest is printed by `brisk assess --backend host`. The harness asks
// Questions about State with its own decision model and writes HostAnswers.
type HostRequest struct {
	Schema    string              `json:"schema"`
	RequestID string              `json:"request_id"` // hash of state, questions, base commit, and whole diff
	Model     string              `json:"model"`      // model the calibration expects, e.g. "typesafe/jev"
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
	// Floor is the rules-only result, so a harness can show something
	// before it has answers or when answering fails.
	Floor Assessment `json:"floor_assessment"`
}

// HostAnswers is read by `brisk assess --answers answers.json`.
type HostAnswers struct {
	Schema    string            `json:"schema"`
	RequestID string            `json:"request_id"`
	Model     string            `json:"model"` // exact version that answered, e.g. "jev-1.14.0"
	Answers   map[string]Answer `json:"answers"`
	Usage     *Usage            `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Language backends

// FindingKind names a kind of structural evidence. The engine decides how
// much each kind matters; language backends only report what they see.
type FindingKind string

const (
	// An exported (public) function, method, type, or field signature changed.
	ExportedSignatureChanged FindingKind = "exported_signature_changed"
	// An exported symbol was removed or renamed.
	ExportedSymbolRemoved FindingKind = "exported_symbol_removed"
	// A condition, return value, or early return changed inside a function
	// whose name or file suggests authentication or authorisation.
	AuthLogicChanged FindingKind = "auth_logic_changed"
	// A test assertion (t.Error*, t.Fatal*, require.*, assert.*, expect(...))
	// was removed or weakened.
	AssertionRemoved FindingKind = "assertion_removed"
	// A test was skipped or focused (t.Skip, it.skip, describe.skip, .only).
	TestSkipped FindingKind = "test_skipped"
	// A test function or test case was deleted.
	TestDeleted FindingKind = "test_deleted"
	// An error is now ignored: `_ = f()`, `if err != nil {}` with no
	// handling, an empty catch, or a catch that only logs.
	ErrorSwallowed FindingKind = "error_swallowed"
	// Concurrency primitives changed: goroutines, channels, mutexes,
	// sync/atomic, Promise.all/race, React effect dependencies.
	ConcurrencyChanged FindingKind = "concurrency_changed"
	// SQL in code strings changed (queries, not migrations).
	SQLChanged FindingKind = "sql_changed"
	// The file could not be parsed; Detail says why.
	ParseError FindingKind = "parse_error"
)

// Finding is one piece of structural evidence in one file.
type Finding struct {
	Kind      FindingKind `json:"kind"`
	Path      string      `json:"path"`
	StartLine int         `json:"start_line,omitempty"` // in the new file; old file for removals
	EndLine   int         `json:"end_line,omitempty"`
	Symbol    string      `json:"symbol,omitempty"` // e.g. "CanAccess" or "(*Server).Handle"
	Detail    string      `json:"detail"`           // one short sentence for people
	Language  string      `json:"language"`         // "go", "typescript", "tsx"
}

// LanguageBackend reports structural evidence for changed files.
type LanguageBackend interface {
	// Name is e.g. "go" or "typescript".
	Name() string
	// Supports reports whether this backend handles the file at path.
	Supports(path string) bool
	// Analyze inspects one file. It must not touch the filesystem or git:
	// everything it needs is in fc. It returns findings in any order.
	Analyze(ctx context.Context, fc FileChange) ([]Finding, error)
}

// ---------------------------------------------------------------------------
// Context providers

// ContextQuery asks for context about the files in a change.
type ContextQuery struct {
	RepoRoot string
	Base     string // history is read up to and including this commit
	Paths    []string
}

// ProjectContext is what a context provider knows about a project.
type ProjectContext struct {
	// SensitivePaths are project-specific rules, such as from .brisk.yaml.
	SensitivePaths []PathRule `json:"sensitive_paths,omitempty"`
	// Files holds history per changed path.
	Files map[string]FileHistory `json:"files,omitempty"`
}

// PathRule marks paths that need at least a given tier.
type PathRule struct {
	ID     string   `json:"id" yaml:"id"`
	Paths  []string `json:"paths" yaml:"paths"` // gitignore-style globs, "**" allowed
	Floor  Tier     `json:"floor" yaml:"floor"`
	Reason string   `json:"reason" yaml:"reason"`
}

// FileHistory is the history of one path before the change.
type FileHistory struct {
	Owners        []string `json:"owners,omitempty"`
	Commits90d    int      `json:"commits_90d"`    // commits touching the file in the 90 days before base
	Authors90d    int      `json:"authors_90d"`    // distinct authors in that window
	FixCommits    int      `json:"fix_commits"`    // earlier commits touching it whose message says fix
	RevertCommits int      `json:"revert_commits"` // earlier reverts touching it
}

// ContextProvider supplies project context for a change.
type ContextProvider interface {
	Context(ctx context.Context, q ContextQuery) (ProjectContext, error)
}
