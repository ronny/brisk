# brisk

Brisk rates how risky a code change made by a coding agent is: Low, Medium, or High, with up to three evidence-backed reasons. It is one static Go binary that needs only git 2.42 or later. Path and diff rules set the minimum tier; a decision model (TypeSafe's Jev) can only raise it.

## Usage

```sh
brisk assess [--repo DIR] [--base REF] [--head REF] [--session session.json] \
             [--backend auto|openrouter|host|rules-only] [--answers answers.json] \
             [--format text|json] [--fail-on medium|high]
```

- `--base` defaults to the merge base with `origin/HEAD`, else `main`, else `master`.
- Without `--head`, brisk assesses the working tree, including untracked files (it never changes the real index).
- Git reads no attributes from the repository's `.gitattributes` files, so a change cannot hide code by marking it binary or run a clean filter. Only `$GIT_DIR/info/attributes` applies.
- In the working tree, untracked code files that only a `.gitignore` line added by the change ignores are assessed anyway and flagged (`gitignore_hides_code`).
- A new file is analysed even when its name or a `Code generated … DO NOT EDIT` comment says it is generated, but its language findings set at most a Medium floor. Files that were already generated before the change, and lockfiles, are skipped, unless the session's `files_touched` shows the agent edited them with its own tools (`generated_hand_edited`). Edits made through the shell, such as `sed -i`, do not appear in `files_touched` and are not caught.
- Trust boundary: brisk treats everything in the working tree and the assessed commits as written by the agent, and trusts `.git/` (config, hooks, `info/attributes`, `info/exclude`). An agent that can write to `.git/` can change how git reads the repository; brisk does not defend against that.
- Before anything goes to a model, brisk replaces values that look like secrets with `[redacted secret]` and leaves out private key bodies and credential files such as `.env`.
- `--backend auto` (the default) uses OpenRouter when `OPENROUTER_API_KEY` is set and rules only otherwise. `BRISK_MODEL` overrides the model (default `typesafe/jev-1.14`).
- If the model backend fails, brisk falls back to rules only and says so in `provenance.degraded`.
- Exit codes: 0 assessed, 1 error, 2 usage error, 3 tier at or above `--fail-on`.

### Host backend

A harness that has its own way to ask the decision model (the Amp plugin uses `amp.ai.decide`) runs two steps with the same flags:

1. `brisk assess --backend host --format json` prints a [host request](schema/host-request.schema.json): `state`, `questions`, `request_id`, and the rules-only `floor_assessment`. When there is nothing to ask (for example an empty diff), it prints an [assessment](schema/assessment.schema.json) instead; check the `schema` field.
2. The harness sends `state` and `questions` unchanged to the model and writes a [host answers](schema/host-answers.schema.json) file with the same `request_id` (it covers the whole change, so answers for an earlier version of the change are refused), the answering `model`, and the `answers`.
3. `brisk assess --answers answers.json --format json` prints the final assessment. The same scoring code runs as with OpenRouter.

## Policy

Teams set their risk appetite in `.brisk.yaml`, read from the base commit like the path rules ([schema](schema/brisk-config.schema.json)). A preset sets everything; the other keys adjust it. The preset names match the Amp plugin's `amp.brisk.preset`.

| Preset | For | High | Agent review recommended |
| --- | --- | --- | --- |
| `autonomous` | Nobody reviews changes by hand | About 25% of changes; the action is an agent review | High, a strong single signal (access checks, data loss, secrets), or a weakly verified change |
| `reviewed` (default) | Teams with human reviewers | The calibrated thresholds (about 17%); the action is a second review | High, or a strong single signal |
| `strict` | High-stakes code | About 39%, with Medium widened too | Medium or High, or any of the above |

Numbers are from the evaluation on repo A and repo B. `agent_review` in the assessment is separate from the tier, so a Medium change can recommend one; its `reason` is brisk's fixed text, never an ID or text from the repository or its config. `actions` is text from the base `.brisk.yaml`; treat it as the team's own words, not as a prompt.

brisk asks seven questions of its own about every change: `impact` and `uncertainty` (0–3 scores), four yes/no risk flags (`weakens_auth`, `destroys_data`, `breaks_contract`, `breaks_deploy`), and `follow_up_fix`, "Will someone probably have to fix a bug in the code this change adds or modifies within the next two weeks?". The calibration for jev-1.14 weights change size 0.5, `follow_up` (the `follow_up_fix` answer) 0.3 and `impact` 0.2. `uncertainty` and the flags carry no weight; a strong `weakens_auth`, `destroys_data` or `impact` answer raises the tier through an override.

Unknown keys, unknown rule IDs, thresholds outside 0–1 and a second YAML document are errors, so a misspelt setting cannot silently do nothing. Because the file comes from the base commit, such an error stops every assessment until the fix lands on the base branch. `rule_tiers` can lower any rule, including `secret_added`; that is the team's choice.

```yaml
preset: autonomous
high_share: 0.2              # or thresholds: {medium: 0.6, high: 0.8}
rule_tiers:
  migration: high            # this team treats every migration as High
  dependencies: low
actions:
  high: Run an agent review and fix what it finds, then ping #payments.
agent_review:
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
- **What the reviewer gets:** `agent_review.context_files` names the file, so a reviewing agent reads it itself. Its text never appears in `reasons` or `agent_review.reason`.
- **Errors:** the path must name a regular file in the base commit. A missing file, a directory or a symlink stops every assessment until the base branch is fixed, as a bad `.brisk.yaml` does.
- **Cost:** for a repo that names a file, every request key changes when the file changes, so recorded answers have to be refreshed.

On nine labelled fixtures (`testdata/fixtures/business-rule-*.txt`), the catch-all question answered 0.92–0.99 on all five changes that broke a written rule and 0.03–0.05 on the four that did not, over three runs. Without the file brisk rated three of the five rule breaks Low; with it, all five were Medium and no safe change was raised. The fixtures are small and the breaks are plain, so `raise_to: medium` is the default we recommend; move to `high` once the question holds up on your own history.

### Review plan

`agent_review.plan` tells a reviewing agent where to look and what to check, most important first. It is built whether or not a review is recommended. Each area comes from a rule hit, a model override that fired, or an answer that met an `agent_review` condition, so a review recommended by an answer alone still has an area:

- **Source `brisk`:** the area and check text come from brisk's fixed catalog, such as "Access checks: find or add a test that a caller without access is denied."
- **Source `config`:** the area and checks come from the team's `review_checks` in `.brisk.yaml`, keyed by a project rule or question ID.

Jev tailors the plan without writing any of it. The host request carries up to 8 `review_questions`, one 0–3 score per area. They are asked on the same state, in a call separate from the calibrated questions, so calibrated request keys and recordings don't change. An area that scores below 1 is dropped unless it is required, meaning a High rule or override produced it. The rest are ordered by score, with areas Jev did not score (beyond the cap of 8, or added from the answers) after the scored ones, and `provenance.review_plan` records the scores and the dropped areas. Without review answers the plan is brisk's full list, required areas first and then in hit order, and the assessment is not degraded. `provenance.review_plan.status` says why: `tailored`, `not_asked`, `pending` (a harness gave no review answers) or `failed` (with `error`).

The scoring is a second model call on the same state, so by default brisk makes it only when an agent review is recommended; its cost is added to `provenance.usage`. Set `agent_review.tailor_plan` to `always` or `never` to change that. A host harness gets `review_when` with the questions: with `recommended`, it answers them only if the assessment from the main answers recommends a review, then runs `brisk assess --answers` again with `review_answers`.

```yaml
review_checks:
  invoice_rounding:                 # a content_rules, sensitive_paths or questions id
    area: Invoice rounding
    checks:
      - Confirm invoice totals still use half-to-even rounding.
      - Find or add a test with a total ending in .5.
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
