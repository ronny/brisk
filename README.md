# brisk

brisk tells you which changes made by a coding agent need a person to look at them. It assumes every change already gets one review, usually by another agent, before it ships. For each change, `human_check` says whether to bring a person in too, and gives them at most two questions to think about. A preset sets how often it asks: `light`, `balanced` (the default) or `careful`.

Underneath, brisk rates how risky the change is: Low, Medium or High, with up to three evidence-backed reasons. Path and diff rules set the minimum tier, and a decision model, TypeSafe's Jev, can only raise it. brisk is one static Go binary that needs only git 2.42 or later. It runs from the command line, in CI, in Amp through the [Amp plugin](adapters/amp/README.md), or in Claude Code through the [Claude Code plugin](adapters/claude-code/README.md).

For a change that removes an access check from a Go handler, brisk prints:

```text
Risk: HIGH (rules floor: high)  impact 3.0/3  uncertainty 3.0/3
1 files, +0 -5
  - Changes logic inside an access check [language lang:auth_logic_changed]
      backend/internal/web/documents.go:13 (*Server).EditDocument
  - May weaken access checks (model: 0.97) [question weakens_auth]
  - High impact if wrong (model: 3.0 of 3) [question impact]
Action: After the agent review, ask a person to think through the points under human_check before shipping.
Bring a person in: The change is High risk.
  ? Who should be allowed to do this, and does the change still enforce it?
```

The [explainer](docs/explainer/index.html) walks six example changes like this one through brisk. Download it and open it in a browser. [docs/architecture.md](docs/architecture.md) describes the design and its trust boundaries.

## Why brisk?

Most changes an agent makes are fine after one review, and a few need a person. brisk's main job is to pick out those few: it flags the changes that need your attention, says why, and tells you what to think about. We expect most teams to use it this way.

A team that does not run an agent review on every change, for example to save cost, can use brisk one level down. The tier and the review plan (`agent_review.plan`) say when an agent review is worth running and what it should check, such as access checks or stored data. We expect this to be the rarer case.

A team that already reviews every change by hand gets little from brisk beyond its questions.

### Why not let the coding agent flag its own changes?

The coding agent could decide for itself when to ask you. brisk is a separate binary with fixed rules and a separate model for these reasons:

- **It always runs.** The Amp plugin assesses every change before Ship, and the Claude Code plugin after every turn, so a check does not depend on the agent remembering to make one.
- **It is a second opinion.** The agent that wrote the change would be grading its own work. Jev sees only the change and a short record of the session, and answers fixed questions about it.
- **It is consistent.** The rules fire the same way on every run. Jev gives nearly the same answer for the same change: on 200 changes from the evaluation, each scored five times, the model risk moved by at most 0.008 for 95% of them, and 1 of the 200 changed tier. This was measured before brisk asked `follow_up_fix`, and has not been rerun since.
- **It is tuned on real history.** Jev's answers are weighted against past changes that later needed a fix, so brisk ranks changes better than its rules alone. On the evaluation's held-out changes, its PR-AUC is 0.356, against 0.283 for the rules alone and 0.15 for a random order.
- **It is not tied to Amp.** The same binary runs in CI, from a script, in Claude Code through its [plugin](adapters/claude-code/README.md), or under another agent through the [host backend](#host-backend).

We have not yet measured brisk against the coding agent's own judgement. The evaluation compares brisk with its own rules and with diff size, not with an agent asked to flag its own changes. The assessment log (see [Usage](#usage)) records every decision, so real use can answer this.

## How it works

1. **Collect the change.** brisk diffs the change against its base commit, including uncommitted and untracked files, and reads an optional record of the agent's session. All of it is evidence, not instructions.
2. **Set a floor with rules.** Path, diff and session rules, structural findings from tree-sitter parsers for Go and TypeScript, and the team's own rules in the base commit's `.brisk.yaml` each have a tier. The highest tier hit is the floor, such as High for removed access-check logic or destructive SQL.
3. **Ask Jev fixed questions.** brisk sends Jev a budgeted summary of the change, with secrets redacted, and seven questions: impact, uncertainty, four yes/no risk flags, and whether the change will need a bug fix within two weeks. A calibration table, fitted on past changes, turns change size and the answers into a model tier. The final tier is the higher of the floor and the model tier, so Jev can raise the tier but never lower it. Without a model, brisk uses the rules alone.
4. **Plan the review.** Each rule hit, model override that fired, and answer that asks for a person adds a review area, such as access checks or stored data, with checks from brisk's fixed catalog or the team's `review_checks`. In a second call, Jev scores each area 0–3; brisk drops areas below 1 unless a High rule or override requires them, and orders the rest by score.
5. **Decide on a person.** The preset's conditions (a tier, a rule hit, a strong answer, or high uncertainty) decide `human_check.needed`. The top review areas supply its one or two questions.
6. **Report and log.** brisk prints the assessment as text or JSON and appends it to the local assessment log. In CI, `--fail-on` turns the tier into an exit code. In Amp, the plugin labels the thread, guides the agent review, and asks a person before Ship when `human_check` says so. In Claude Code, the plugin shows the result after each turn, runs the agent review before a push, and asks a person in the permission prompt when `human_check` says so.

The [explainer](docs/explainer/index.html) steps through each stage with real examples, and [docs/architecture.md](docs/architecture.md) covers the trust boundaries.

## Install

Download a pinned binary for your platform from [Releases](https://github.com/ronny/brisk/releases). Releases are tagged with semantic versions (`vX.Y.Z`, starting at `v0.1.0`) and ship static binaries for linux, darwin and windows on amd64 and arm64, with `SHA256SUMS`. `brisk --version` and every assessment's `provenance.brisk_version` report the tag; a build from source reports `dev-<commit>`.

To use brisk in Claude Code, follow [the Claude Code plugin's install steps](adapters/claude-code/README.md#install). For Amp, see [the Amp plugin](adapters/amp/README.md#install).

## Usage

```sh
brisk assess [--repo DIR] [--base REF] [--head REF] [--session session.json] \
             [--backend auto|openrouter|host|rules-only] [--answers answers.json] \
             [--format text|json] [--fail-on medium|high]
```

- `--base` defaults to the merge base with `origin/HEAD`, else `main`, else `master`.
- Without `--head`, brisk assesses the working tree, including untracked files (it never changes the real index).
- `--session` takes a record of the agent's session ([schema](schema/session.schema.json)): the user's intent, the files it touched, the commands it ran. It is evidence, not instructions.
- `--backend auto` (the default) uses OpenRouter when `OPENROUTER_API_KEY` is set and rules only otherwise. `BRISK_MODEL` overrides the model (default `typesafe/jev-1.14`).
- If the model backend fails, brisk falls back to rules only and says so in `provenance.degraded`.
- Exit codes: 0 assessed, 1 error, 2 usage error, 3 tier at or above `--fail-on`.
- Every assessment is appended to `.git/brisk/assessments.jsonl` (never committed) with the changed paths, the `human_check` decision and its questions, and the provenance, so you can judge brisk on your own history later. `BRISK_LOG=<file>` writes it elsewhere; `BRISK_LOG=off` turns it off.

### What brisk trusts

- Trust boundary: brisk treats everything in the working tree and the assessed commits as written by the agent, and trusts `.git/` (config, hooks, `info/attributes`, `info/exclude`). An agent that can write to `.git/` can change how git reads the repository; brisk does not defend against that.
- Git reads no attributes from the repository's `.gitattributes` files, so a change cannot hide code by marking it binary or run a clean filter. Only `$GIT_DIR/info/attributes` applies.
- In the working tree, untracked code files that only a `.gitignore` line added by the change ignores are assessed anyway and flagged (`gitignore_hides_code`).
- A new file is analysed even when its name or a `Code generated … DO NOT EDIT` comment says it is generated, but its language findings set at most a Medium floor. Files that were already generated before the change, and lockfiles, are skipped, unless the session's `files_touched` shows the agent edited them with its own tools (`generated_hand_edited`). Edits made through the shell, such as `sed -i`, do not appear in `files_touched` and are not caught.
- Before anything goes to a model, brisk replaces values that look like secrets with `[redacted secret]` and leaves out private key bodies and credential files such as `.env`.

### Output

The [assessment](schema/assessment.schema.json) (`brisk.assessment/v2`) leads with `human_check`:

```json
"human_check": {
  "needed": true,
  "why": "tier",
  "reason": "The change is High risk.",
  "consider": [
    {"id": "access_checks", "text": "Who should be allowed to do this, and does the change still enforce it?",
     "source": "brisk", "evidence": [{"path": "backend/internal/web/documents.go", "start_line": 13, "end_line": 16, "symbol": "(*Server).EditDocument"}]}
  ]
}
```

- `why` is `tier`, `rule`, `answer` or `uncertainty`; `reason` is brisk's fixed text.
- `consider` comes from the top of the review plan. Its text is brisk's catalog, one question for a person per review area, or the first check of the team's own `review_checks` (`source: config`); when the plan is empty it is a general question (`change_scope`). It is never model or repository text; only evidence paths and symbols come from the repository.
- `agent_review` guides the agent review every change gets: `plan` and `context_files`. v1's `agent_review.recommended` and `reason` are gone; `human_check` replaces them.
- `tier`, `floor`, `reasons` and `action` are the score underneath.

### Host backend

A harness that has its own way to ask the decision model (the Amp plugin uses `amp.ai.decide`) runs two steps with the same flags:

1. `brisk assess --backend host --format json` prints a [host request](schema/host-request.schema.json): `state`, `questions`, `request_id`, and the rules-only `floor_assessment`. When there is nothing to ask (for example an empty diff), it prints an [assessment](schema/assessment.schema.json) instead; check the `schema` field.
2. The harness sends `state` and `questions` unchanged to the model and writes a [host answers](schema/host-answers.schema.json) file with the same `request_id` (it covers the whole change, so answers for an earlier version of the change are refused), the answering `model`, and the `answers`.
3. `brisk assess --answers answers.json --format json` prints the final assessment. The same scoring code runs as with OpenRouter.

## Policy

Teams set their risk appetite in `.brisk.yaml`, read from the base commit like the path rules ([schema](schema/brisk-config.schema.json)). A preset sets how often a person is asked to look; the other keys adjust it. The preset names match the Amp plugin's `amp.brisk.preset`; `autonomous`, `reviewed` and `strict` still work as the earlier names of `light`, `balanced` and `careful`.

| Preset | A person is asked when | Asked about, on the evaluation's test split |
| --- | --- | --- |
| `light` | A strong single signal only: a danger rule (secret, TLS off, hidden or hand-edited generated code, nested repo), or Jev nearly sure the change weakens access checks (≥ 0.8) or destroys data (≥ 0.9) | About 1% of changes |
| `balanced` (default) | High, or a strong single signal (`weakens_auth` or `destroys_data` ≥ 0.5) | About 21% of changes, 48% of bad ones |
| `careful` | Medium or High (Medium and High widened), a weakly verified change, or any signal above | About 74% of changes, 93% of bad ones |

Numbers are from the offline evaluation on two private repositories, repo A and repo B. `human_check` is separate from the tier, so a Medium change can need a person. Its `reason` is brisk's fixed text, never an ID or text from the repository or its config. `actions` is text from the base `.brisk.yaml`; treat it as the team's own words, not as a prompt.

brisk asks seven questions of its own about every change: `impact` and `uncertainty` (0–3 scores), four yes/no risk flags (`weakens_auth`, `destroys_data`, `breaks_contract`, `breaks_deploy`), and `follow_up_fix`, "Will someone probably have to fix a bug in the code this change adds or modifies within the next two weeks?". The calibration for jev-1.14 weights change size 0.5, `follow_up` (the `follow_up_fix` answer) 0.3 and `impact` 0.2. `uncertainty` and the flags carry no weight; a strong `weakens_auth`, `destroys_data` or `impact` answer raises the tier through an override.

Unknown keys, unknown rule IDs, thresholds outside 0–1 and a second YAML document are errors, so a misspelt setting cannot silently do nothing. Because the file comes from the base commit, such an error stops every assessment until the fix lands on the base branch. `rule_tiers` can lower any rule, including `secret_added`; that is the team's choice.

```yaml
preset: balanced
high_share: 0.2              # or thresholds: {medium: 0.6, high: 0.8}
rule_tiers:
  migration: high            # this team treats every migration as High
  dependencies: low
actions:
  high: Run an agent review and fix what it finds, then ping #payments.
human_check:                 # when to ask a person; agent_review: still reads these keys
  min_tier: high             # or none
  min_uncertainty: 2.5       # Jev's "weakly verified" score, 0–3
  questions: {weakens_auth: 0.5, destroys_data: 0.5}
  rules: [secret_added, ledger]
sensitive_paths:
  - id: ledger
    paths: ["internal/ledger/**"]
    floor: high
```

### Project rules

A repo adds its own checks in the same file. `sensitive_paths` and `content_rules` are deterministic and set the floor; `questions` are asked of the model with brisk's own questions and can only raise the tier. Project questions have no calibration weight, so they act only through `raise_to`; without it the answer is just recorded in `provenance.answers`.

```yaml
content_rules:
  - id: invoice_rounding
    paths: ["internal/invoice/**"]
    pattern: 'math\.(Round|Floor|Ceil)|RoundingMode'   # Go RE2, per changed line
    lines: added                                       # or removed, both
    floor: medium
    reason: Invoice totals must use banker's rounding (FR-12).
questions:
  - id: invoice_rounding_changed
    text: Does this change how invoice totals or tax amounts are rounded?
    raise_to: high
    min: 0.8
    reason: Changes invoice rounding (FR-12).
```

A content rule matches one line at a time, so it is a tripwire, not a guarantee: a call split across lines or an import alias slips past it. Pair an important one with a question.

Test a project rule with a fixture in `testdata/fixtures/` (a base `.brisk.yaml`, base and head files, and the expected tier), or with a throwaway commit and `brisk assess --base HEAD`.

### Business rules

Write the project's business rules in plain prose in a Markdown file and name it in `.brisk.yaml`:

```yaml
business_rules: .brisk/business-rules.md
questions:
  - id: breaks_business_rule
    text: Does this change break or alter a project business rule listed in the state?
    raise_to: medium
```

- **Where it comes from:** like `.brisk.yaml`, the file is read from the base commit, so a change cannot rewrite the rules it is judged against. Editing `.brisk/**` hits the `brisk_config` rule (Medium).
- **What Jev sees:** the first 4,000 characters (cut at a line end) go into the model state as `project_business_rules`, labelled as the team's rules and not as instructions, with secrets redacted. The rest of the state budget stays with the change.
- **What sets the tier:** only `questions` and `content_rules`. The file itself never raises or lowers anything; pair it with a catch-all question like the one above, or one question per rule that matters.
- **What the reviewer gets:** `agent_review.context_files` names the file, so a reviewing agent reads it itself. Its text never appears in `reasons` or `human_check`.
- **Errors:** the path must name a regular file in the base commit. A missing file, a directory or a symlink stops every assessment until the base branch is fixed, as a bad `.brisk.yaml` does.
- **Cost:** for a repo that names a file, every request key changes when the file changes, so recorded answers have to be refreshed.

On nine labelled fixtures (`testdata/fixtures/business-rule-*.txt`), the catch-all question answered 0.92–0.99 on all five changes that broke a written rule and 0.03–0.05 on the four that did not, over three runs. Without the file brisk rated three of the five rule breaks Low; with it, all five were Medium and no safe change was raised. The fixtures are small and the breaks are plain, so `raise_to: medium` is the default we recommend; move to `high` once the question holds up on your own history.

### Review plan

`agent_review.plan` tells the reviewing agent where to look and what to check, most important first, and its top areas become `human_check.consider`. Each area comes from a rule hit, a model override that fired, or an answer that met a `human_check` condition, so a person asked for by an answer alone still has something to consider:

- **Source `brisk`:** the area and check text come from brisk's fixed catalog, such as "Access checks: find or add a test that a caller without access is denied."
- **Source `config`:** the area and checks come from the team's `review_checks` in `.brisk.yaml`, keyed by a project rule or question ID.

Jev tailors the plan without writing any of it. The host request carries up to 8 `review_questions`, one 0–3 score per area. They are asked on the same state, in a call separate from the calibrated questions, so calibrated request keys and recordings don't change. An area that scores below 1 is dropped unless it is required, meaning a High rule or override produced it. The rest are ordered by score, with areas Jev did not score (beyond the cap of 8, or added from the answers) after the scored ones, and `provenance.review_plan` records the scores and the dropped areas. Without review answers the plan is brisk's full list, required areas first and then in hit order, and the assessment is not degraded. `provenance.review_plan.status` says why: `tailored`, `not_asked`, `pending` (a harness gave no review answers) or `failed` (with `error`).

The scoring is a second model call on the same state. Because every change gets an agent review, brisk makes it by default (`agent_review.tailor_plan: always`), alongside the main call; its cost is added to `provenance.usage`. Set `tailor_plan` to `needed` (only when a person is asked; `recommended` is the earlier name) or `never` to save it. A host harness gets `review_when` with the questions: with `always` it answers them with the main questions; with `needed` it answers them only if the assessment from the main answers has `human_check.needed`, then runs `brisk assess --answers` again with `review_answers`.

```yaml
review_checks:
  invoice_rounding:                 # a content_rules, sensitive_paths or questions id
    area: Invoice rounding
    checks:
      - Confirm invoice totals still use half-to-even rounding.
      - Find or add a test with a total ending in .5.
```

## Evaluation repositories

The offline evaluation (`cmd/brisk-eval`) runs on the history of private repositories, which this repository never names. `brisk-eval` takes each repository only as a `-repo DIR` flag. Keep the local paths in `eval/repos.env`, which git ignores:

```sh
REPO_A=/path/to/first/repository
REPO_B=/path/to/second/repository
```

Load it before you run the evaluation, and give each repository a neutral `-name`, because that name goes into the labels, reports and calibration notes:

```sh
set -a; . eval/repos.env; set +a
brisk-eval label -repo "$REPO_A" -name repo-a -out /tmp/eval/a.labels.jsonl
```

## Documents

| Schema | Go type |
| --- | --- |
| [session.schema.json](schema/session.schema.json) | `risk.Session` |
| [assessment.schema.json](schema/assessment.schema.json) | `risk.Assessment` |
| [host-request.schema.json](schema/host-request.schema.json) | `risk.HostRequest` |
| [host-answers.schema.json](schema/host-answers.schema.json) | `risk.HostAnswers` |
| [brisk-config.schema.json](schema/brisk-config.schema.json) | `.brisk.yaml`: `risk.PolicyConfig` and path rules |

## Layout

| Path | What |
| --- | --- |
| `risk/` | Shared types and interfaces (`Backend`, `ContextProvider`, `LanguageBackend`). Standard library only |
| `assess/` | The engine: diff, rules, language findings, questions, tiering |
| `rules/` | Path and diff rules that set the floor |
| `backend/` | `openrouter` (Jev) and `host` backends |
| `context/gitctx/` | Context from `.brisk.yaml`, CODEOWNERS, and git history |
| `lang/` | Tree-sitter language backends for Go and TypeScript/TSX. Exports `lang.Backends() []risk.LanguageBackend` |
| `calibration/` | Thresholds per backend and model version, as data |
| `eval/`, `cmd/brisk-eval/` | Offline evaluation on git history |
| `adapters/amp/` | The Amp plugin |
| `.amp/plugins/brisk/`, `.brisk.yaml`, `scripts/install-brisk.sh` | How Amp threads on this repository are rated: a copy of `adapters/amp/` and the binary from the release in `BRISK_VERSION`, so a change to brisk's code does not change how it is rated. Changes to these files ask for a person |
