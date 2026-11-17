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
- A new file is analysed even when its name looks generated (`*_gen.go`, `*.pb.go`); only files that already had such a name, and lockfiles, are skipped.
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

## Documents

| Schema | Go type |
| --- | --- |
| [session.schema.json](schema/session.schema.json) | `risk.Session` |
| [assessment.schema.json](schema/assessment.schema.json) | `risk.Assessment` |
| [host-request.schema.json](schema/host-request.schema.json) | `risk.HostRequest` |
| [host-answers.schema.json](schema/host-answers.schema.json) | `risk.HostAnswers` |

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
