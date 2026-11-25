# brisk for Claude Code

A Claude Code plugin built on the `brisk` binary. Every change gets one agent review before it ships, guided by brisk's review plan. brisk decides whether a person should look too, and what they should think about:

- **After each turn** (`Stop` hook), it assesses the working tree against the base and shows you one line: the risk, whether a model answered, and the reasons. When brisk's `human_check` asks for a person, the message adds brisk's one or two questions ("Think about: …"). It shows a message only when the result changed, so a turn that changed no code shows nothing.
- **Before a push** (`PreToolUse` hook on Bash), for `git push` (with git options such as `-C dir`), `gh pr create` and `gh pr merge`:
  1. The first push of each version of the changes is denied, with brisk's review message: hand the changes to a reviewing subagent with brisk's plan, fix what it finds, then push again (`agent_review: before-push`, the default).
  2. When brisk asks for a person, the push goes to Claude Code's permission prompt with brisk's questions. Only a person can answer it. In `-p` runs and in `dontAsk` mode, Claude Code turns the prompt into a denial, and Claude reads the questions in the reason.
  3. Otherwise the hook stays silent and Claude Code's own permission rules decide. It never approves a tool call.
- brisk appends every assessment to `.git/brisk/assessments.jsonl`, never committed, as it does for the CLI. The plugin keeps its per-session state (the last result and which versions were reviewed) in `.git/brisk/claude-code/`.

The hooks build a [session record](../../schema/session.schema.json) from the session's transcript: your first message as the intent, your later messages (they hold corrections), the files Claude edited with its own tools, the shell commands it ran with their exit codes, and its last message. Claude Code does not document its transcript format; entries the plugin does not recognise are skipped, which makes a change look less verified and errs toward asking a person.

## What does not carry over from the Amp plugin

| Amp plugin | Claude Code |
| --- | --- |
| Thread labels `brisk-risk-*`, `brisk-needs-human` | No labels: the message after each turn, and the assessment log |
| Brisk assessment artifact | No artifacts: run `brisk assess` for the full result |
| The person-only dialog before Ship | The permission prompt before a push |
| Ship and Push to Branch | Only pushes Claude makes through Bash are seen. A push you make in your own terminal is not, so the message after each turn is the main signal |
| Jev through `amp.ai.decide`, no key needed | Jev only through OpenRouter (below) |
| Quick actions and the command palette entry | None yet |

## How the model questions are answered

Claude Code has no route to Jev of its own, so brisk calls OpenRouter itself when it has a key: the plugin's `openrouter_api_key` option first, then `OPENROUTER_API_KEY` from the environment. Each new version of the changes costs one Jev call, plus the plan-tailoring call while `agent_review.tailor_plan` is `always` (brisk's default), billed to that OpenRouter account. An unchanged diff is never asked twice in a session. A failed call falls back to rules only, says so, and is tried again on the next hook.

Without a key the plugin runs **rules only**: the tier is the rules floor, `human_check` fires only on rule hits and the floor tier, and the review plan is not tailored. Every message then says "rules only". The README's numbers for how often brisk asks are for Jev; on the evaluation, rules alone rank changes at PR-AUC 0.283 against 0.356 with Jev.

## Settings

Claude Code asks for these when you enable the plugin, and `/config` lists them. They are stored in your user settings (the key in the system's credential store) and reach the hooks as `CLAUDE_PLUGIN_OPTION_<KEY>`.

| Option | Default | |
| --- | --- | --- |
| `binary` | `brisk` | A command name, looked up on `PATH` skipping relative entries and entries inside the project, or an absolute or `~/` path outside the project |
| `base` | brisk's default | Base ref to compare the working tree against |
| `agent_review` | `before-push` | `before-push`; `after-turn`, which also keeps Claude working for one review when its turn ends (once per version of the changes, never right after such a review, and with brisk's own plan areas only, so a push still asks for the project's areas); or `off` |
| `ask_person` | `ask` | `ask` (the questions after a turn, and the permission prompt before a push), `note` (the questions only), or `off` |
| `confirm_unassessed` | `false` | Ask a person before a push when brisk could not assess the changes |
| `openrouter_api_key` | none | See above |

An invalid value is reported after each turn, and every push then asks a person until it is fixed.

## Install

1. Install brisk v0.2.0 or later, the first release with the `claude-code` command. Download the binary for your platform from [Releases](https://github.com/ronny/brisk/releases), named `brisk-<tag>-<os>-<arch>`, check it against the release's `SHA256SUMS`, and keep it outside your projects. For example, on an Apple silicon Mac:

   ```sh
   mkdir -p ~/.local/bin
   curl -fsSL -o ~/.local/bin/brisk https://github.com/ronny/brisk/releases/download/v0.2.0/brisk-v0.2.0-darwin-arm64
   chmod +x ~/.local/bin/brisk
   ~/.local/bin/brisk --version   # brisk v0.2.0
   ```

2. In Claude Code, add this repository as a plugin marketplace and install the plugin:

   ```text
   /plugin marketplace add ronny/brisk
   /plugin install brisk@brisk
   ```

   To try a local checkout instead, start Claude Code with `claude --plugin-dir <checkout>/adapters/claude-code`.
3. Set the plugin's options when Claude Code asks, or later in `/config` (see [Settings](#settings)):
   - `binary`: the absolute path from step 1, such as `~/.local/bin/brisk`. Hooks may not get your shell's `PATH`, so a path is more reliable than `brisk`.
   - `openrouter_api_key`: your OpenRouter key, so Jev answers brisk's questions. Without it the plugin runs rules only.
   - Leave the others at their defaults to start.
4. Try it in a git repository: ask Claude to change some code, then to push.
   - After the turn, a `brisk: Risk …` message shows the tier, and brisk's questions when a person should look.
   - The first push is denied, with brisk's review plan for Claude to work through.
   - The next push shows Claude Code's permission prompt when brisk asks for a person; approve it to push.
   - Nothing at all? The hook failed. `claude --debug` shows hook errors, such as `no brisk on PATH`.
5. Add the `agent_config` rule from [What it cannot defend against](#what-it-cannot-defend-against) to the base `.brisk.yaml` of the repositories you use it in.

The hook wrapper is a POSIX `sh` script, so on Windows the plugin needs Git Bash.

## What it cannot defend against

- **The repository can reconfigure Claude Code.** A project's `.claude/settings.json` can turn every hook off (`disableAllHooks`), set environment variables for them, and add hooks of its own. A change that edits it takes effect in the next session. The plugin cannot ignore project settings the way the Amp plugin ignores `.amp/settings.json`. Have brisk ask a person about such changes with a rule in your base `.brisk.yaml`:

  ```yaml
  sensitive_paths:
    - id: agent_config
      paths: [".claude/**", ".mcp.json", "**/CLAUDE.md"]
      floor: high
      reason: Changes how the coding agent and its hooks run
  ```

  `balanced` and `careful` ask a person about every High change. Under `light`, also list the rule in `human_check.rules`, next to light's own rules, because setting `rules` replaces the preset's list:

  ```yaml
  human_check:
    rules: [secret_added, tls_disabled, gitignore_hides_code, nested_repo, generated_without_source, generated_hand_edited, agent_config]
  ```

- **The push gate matches common commands only.** A push through a script, an alias or another tool is not seen. Words inside a quoted commit message can look like a push, which costs one review request.
- **It assesses the working tree, not what is pushed.** Before a push it rates the checked-out working tree against the base, like after a turn. A push of another branch from a clean checkout, `gh pr merge` from a clean checkout, or `git -C` into another repository is rated on this checkout, or not at all when it has no changes.
- **A hook that fails lets the push through.** Claude Code treats a missing binary, unreadable input, or a hook over its 120 s timeout as a non-blocking error, even with `confirm_unassessed`. For a hard stop, run `brisk assess --fail-on high` in CI.
- Reasons quote the code, so every reason, question and check the plugin shows or sends to Claude is collapsed to one line of at most 300 characters (200 for reasons), and the review message tells the reviewer to treat paths and symbols as data.
- `BRISK_OPENROUTER_URL` is ignored, so a project's settings cannot send your key elsewhere. `BRISK_MODEL` is read, because a project could only pick another model, which cannot lower the rules floor.

## Develop

The hook logic is `brisk claude-code` in [`cmd/brisk/claudecode.go`](../../cmd/brisk/claudecode.go). It reads the hook input on stdin and prints the hook output. Its tests run the wrapper and the built binary against recorded hook inputs and transcripts, with rules only or a local fake of OpenRouter:

```sh
go test ./cmd/brisk -run 'ClaudeCode|IsPush'
```
