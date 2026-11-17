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
	"regexp"
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
	Schema      string      `json:"schema"`
	Tier        Tier        `json:"tier"`
	Floor       Tier        `json:"floor"`       // minimum tier set by rules
	Impact      float64     `json:"impact"`      // 0–3
	Uncertainty float64     `json:"uncertainty"` // 0–3
	Action      string      `json:"action"`
	AgentReview AgentReview `json:"agent_review"`
	Reasons     []Reason    `json:"reasons"` // at most 3, strongest first
	Stats       Stats       `json:"stats"`
	Provenance  Provenance  `json:"provenance"`
}

// AgentReview says whether brisk recommends that a reviewing agent check
// the change before it ships, separately from the tier: a Medium change
// can recommend one. Reason is one sentence written by brisk, never text
// from the repository.
type AgentReview struct {
	Recommended bool   `json:"recommended"`
	Reason      string `json:"reason"`
	// Plan lists where a reviewer should look and what to check, most
	// important first. It is built whether or not a review is recommended.
	Plan []ReviewArea `json:"plan"`
}

// ReviewArea is one part of a review plan. Area and check text come from
// brisk's catalog (Source "brisk") or the base .brisk.yaml ("config"); the
// model never writes it. Evidence paths and symbols are repository text.
type ReviewArea struct {
	ID       string        `json:"id"`
	Area     string        `json:"area"`
	Source   string        `json:"source"`
	Required bool          `json:"required"`        // a High rule or override; never dropped
	Score    *float64      `json:"score,omitempty"` // the model's 0-3 relevance, when tailored
	Evidence []Evidence    `json:"evidence"`
	Checks   []ReviewCheck `json:"checks"`
}

// ReviewCheck is one thing a reviewer should check.
type ReviewCheck struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ReviewPlanProvenance records how the model tailored the review plan.
type ReviewPlanProvenance struct {
	// Status is "tailored" (the model scored the areas), "not_asked" (the
	// policy or backend did not ask), "pending" (a harness was meant to
	// answer review_questions and gave no answers), or "failed".
	Status   string             `json:"status"`
	Error    string             `json:"error,omitempty"` // why the review call failed
	Tailored bool               `json:"tailored"`        // Status == "tailored"
	Scores   map[string]float64 `json:"scores"`
	Dropped  []string           `json:"dropped"` // areas scored below 1, never required ones
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
	BriskVersion string                `json:"brisk_version"`
	Backend      string                `json:"backend"`               // "openrouter", "host", "rules-only"
	Model        string                `json:"model,omitempty"`       // exact version that answered
	Calibration  string                `json:"calibration,omitempty"` // calibration table used
	Preset       string                `json:"preset,omitempty"`      // policy preset from .brisk.yaml
	Thresholds   map[string]float64    `json:"thresholds,omitempty"`  // model risk thresholds the policy used
	PolicyNote   string                `json:"policy_note,omitempty"` // why the policy could not be applied in full
	ReviewPlan   *ReviewPlanProvenance `json:"review_plan,omitempty"`
	Degraded     string                `json:"degraded,omitempty"` // why a backend fell back to rules only
	Base         string                `json:"base"`
	Head         string                `json:"head,omitempty"`
	DiffHash     string                `json:"diff_hash"`
	RuleHits     []string              `json:"rule_hits"`
	Findings     []Finding             `json:"findings,omitempty"`
	Answers      map[string]Answer     `json:"answers,omitempty"`
	ModelRisk    *float64              `json:"model_risk,omitempty"` // combined 0–1 risk from the answers
	Usage        *Usage                `json:"usage,omitempty"`
	DurationMS   int64                 `json:"duration_ms"`
	Timings      map[string]float64    `json:"timings_ms,omitempty"`
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
	// ReviewQuestions score the review plan's areas. Answer them in a
	// separate call on the same state; they are optional.
	ReviewQuestions map[string]Question `json:"review_questions,omitempty"`
	// ReviewWhen says when to answer ReviewQuestions: "always", or
	// "recommended": only when the assessment from the main answers
	// recommends an agent review (then rerun with review_answers).
	ReviewWhen string `json:"review_when,omitempty"`
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
	// ReviewAnswers answer the request's review_questions; optional.
	ReviewAnswers map[string]Answer `json:"review_answers,omitempty"`
	// ReviewError says why the harness's review call failed, if it did.
	ReviewError string `json:"review_error,omitempty"`
	// ReviewUsage is the review call's cost; it is added to usage.
	ReviewUsage *Usage `json:"review_usage,omitempty"`
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
	// ContentRules are project rules on changed lines, from .brisk.yaml.
	ContentRules []ContentRule `json:"content_rules,omitempty"`
	// Policy is the project's risk policy from .brisk.yaml.
	Policy PolicyConfig `json:"policy,omitempty"`
	// Files holds history per changed path.
	Files map[string]FileHistory `json:"files,omitempty"`
}

// PolicyConfig is a team's risk policy from .brisk.yaml. Every field is
// optional: a preset (default "reviewed") fills the rest. Tiers are
// written "low", "medium", "high".
type PolicyConfig struct {
	Preset string `json:"preset,omitempty" yaml:"preset"`
	// HighShare and MediumShare set the model thresholds as the share of
	// changes (0–1) the model alone puts at High, or at Medium or above,
	// using the calibration's risk quantiles.
	HighShare   *float64 `json:"high_share,omitempty" yaml:"high_share"`
	MediumShare *float64 `json:"medium_share,omitempty" yaml:"medium_share"`
	// Thresholds sets model risk thresholds (0–1) directly; they win over
	// the shares.
	Thresholds map[string]float64 `json:"thresholds,omitempty" yaml:"thresholds"`
	// Overrides replace the calibration's: an answer at or above Min
	// raises the model tier to Tier.
	Overrides []OverrideConfig `json:"overrides,omitempty" yaml:"overrides"`
	// RuleTiers changes the tier a rule or finding sets, by hit ID, such
	// as migration: high or dependencies: low.
	RuleTiers map[string]string `json:"rule_tiers,omitempty" yaml:"rule_tiers"`
	// Actions is the text shown for each tier.
	Actions     map[string]string  `json:"actions,omitempty" yaml:"actions"`
	AgentReview *AgentReviewConfig `json:"agent_review,omitempty" yaml:"agent_review"`
	// Questions are the project's own yes/no questions, asked in the same
	// model request as brisk's.
	Questions []QuestionConfig `json:"questions,omitempty" yaml:"questions"`
	// ReviewChecks gives a project rule or question its own review area,
	// keyed by its id.
	ReviewChecks map[string]ReviewChecksConfig `json:"review_checks,omitempty" yaml:"review_checks"`
}

// ReviewChecksConfig is a team's review area for a project rule or question.
type ReviewChecksConfig struct {
	Area   string   `json:"area" yaml:"area"`
	Checks []string `json:"checks" yaml:"checks"`
}

// QuestionConfig is a project's own yes/no question for the model. With
// RaiseTo set, an answer at or above Min (default 0.8) raises the model
// tier to RaiseTo; it can never lower a tier. Reason is shown when it fires.
type QuestionConfig struct {
	ID      string   `json:"id" yaml:"id"`
	Text    string   `json:"text" yaml:"text"`
	RaiseTo string   `json:"raise_to,omitempty" yaml:"raise_to"`
	Min     *float64 `json:"min,omitempty" yaml:"min"`
	Reason  string   `json:"reason,omitempty" yaml:"reason"`
}

// OverrideConfig raises the model tier when one answer is strong enough.
type OverrideConfig struct {
	Question string  `json:"question" yaml:"question"`
	Min      float64 `json:"min" yaml:"min"`
	Tier     string  `json:"tier" yaml:"tier"`
}

// AgentReviewConfig says when brisk recommends an agent review. Any one
// condition is enough; a field left out keeps the preset's value.
type AgentReviewConfig struct {
	// MinTier recommends a review at or above this tier; "none" turns
	// the tier condition off.
	MinTier *string `json:"min_tier,omitempty" yaml:"min_tier"`
	// MinUncertainty recommends one when Jev rates the change at least
	// this weakly verified (0–3); 0 turns it off.
	MinUncertainty *float64 `json:"min_uncertainty,omitempty" yaml:"min_uncertainty"`
	// Questions recommends one when a yes/no answer reaches its minimum.
	Questions map[string]float64 `json:"questions,omitempty" yaml:"questions"`
	// Rules recommends one when any of these hits fires.
	Rules []string `json:"rules,omitempty" yaml:"rules"`
	// TailorPlan says when to ask the model to score the review plan:
	// "recommended" (default: only when a review is recommended),
	// "always", or "never".
	TailorPlan *string `json:"tailor_plan,omitempty" yaml:"tailor_plan"`
}

// PathRule marks paths that need at least a given tier.
type PathRule struct {
	ID     string   `json:"id" yaml:"id"`
	Paths  []string `json:"paths" yaml:"paths"` // gitignore-style globs, "**" allowed
	Floor  Tier     `json:"floor" yaml:"floor"`
	Reason string   `json:"reason" yaml:"reason"`
}

// ContentRule marks changed lines that need at least a given tier: a
// project's own business rule, such as "changes invoice rounding".
type ContentRule struct {
	ID      string         `json:"id"`
	Paths   []string       `json:"paths,omitempty"` // globs; empty means code files
	Pattern *regexp.Regexp `json:"-"`
	Lines   string         `json:"lines,omitempty"` // "added" (default), "removed", or "both"
	Floor   Tier           `json:"floor"`
	Reason  string         `json:"reason,omitempty"`
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
