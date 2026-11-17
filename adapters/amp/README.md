# brisk for Amp

An Amp directory plugin built on the `brisk` binary. Every change gets one agent review before it ships, guided by brisk's review plan; brisk decides whether a person should look too, and what they should think about:

- After each agent turn that edits files or runs commands, it labels the thread `brisk-risk-low`, `brisk-risk-medium`, or `brisk-risk-high`, plus `brisk-needs-human` when brisk's `human_check` asks for a person. It replaces only brisk's own labels, including the unprefixed `risk-*` labels earlier versions set; your other labels stay. Amp labels allow only letters, digits and hyphens, so the namespace is `brisk-`, not `brisk:`.
- It also keeps one **Brisk assessment** artifact in the thread, updated after each assessment: the risk and rules floor, whether a person should look and what to think about, the agent review areas, the reasons, the brisk version, backend, model and preset, and the last 10 assessments. It holds brisk's capped one-line text and file paths, never the diff, the business-rules text or secrets. If the artifact cannot be published, the plugin logs why and the labels still apply.
- It offers a **Start agent review** quick action, and by default makes sure one agent review ran on this version of the changes before Ship (see Agent review).
- When brisk asks for a person, Ship and Push to Branch first show a dialog, answered only by a person, with brisk's one or two questions ("Think about: …"). **I've thought about it, ship** continues; declining cancels the turn. When nobody is watching the thread, the dialog is skipped and the Ship turn stops before pushing and leaves the questions in its reply (or ships and leaves them, with `amp.brisk.unattended: ship`; Amp reports a dismissed dialog the same way as an unwatched thread, so with `ship` closing the dialog also ships).
- For Medium and High it adds a line to the Ship and Push to Branch prompt, such as `Risk: High (Changes CanAccess). Before pushing, review …`.
- Optional, per project in user settings: a confirmation before Ship when brisk could not assess the changes.
- It logs the brisk version with every assessment, and brisk keeps its own log of each assessment in `.git/brisk/assessments.jsonl`.

The plugin builds a [session record](../../schema/session.schema.json) from the thread (intent, edited files, shell commands with exit codes, user corrections, the agent's last summary) and passes it with `--session`.

## How the model questions are answered

| `brisk.backend` | What runs |
| --- | --- |
| `auto` (default) | `openrouter` when `OPENROUTER_API_KEY` is set in the plugin's environment, else `amp` |
| `amp` | `brisk assess --backend host`, then Amp's Jev through `amp.ai.decide`, then `brisk assess --answers`. No OpenRouter key needed |
| `openrouter` | `brisk assess --backend auto`; brisk calls OpenRouter itself |
| `rules-only` | No model |

If `amp.ai.decide` fails (for example the workspace disabled Jev), the plugin shows brisk's rules-only floor and the label text says "rules only".

## Settings

The plugin reads its settings only from your user settings (`~/.config/amp/settings.json` or `.jsonc`) and the machine's managed settings (`/etc/ampcode/managed-settings.json`, or the macOS and Windows equivalents; managed wins). It ignores a repository's `.amp/settings.json`, because the change being rated could edit that file to choose the binary that rates it, lower the backend, or turn the confirmation off.

| Key | Default | |
| --- | --- | --- |
| `amp.brisk.binary` | `brisk` on `PATH` | A command name, or an absolute or `~/` path outside the project. A name is looked up on `PATH`, skipping relative entries and entries inside the project. Other values fall back to `brisk` |
| `amp.brisk.backend` | `auto` | See above |
| `amp.brisk.base` | brisk's default | Base ref to compare against |
| `amp.brisk.preset` | `balanced` | `light`, `balanced`, or `careful` (`autonomous`, `reviewed`, `strict` are the earlier names). Sets the defaults of the two keys below |
| `amp.brisk.confirmUnassessed` | preset | `true` for every project, `false`, or a list of project roots, such as `["~/src/payments"]`. Before Ship or Push to Branch when brisk could not assess the changes, a person must confirm; declining cancels the turn. `amp.brisk.confirmShipOnHigh` is the earlier name |
| `amp.brisk.agentReview` | preset | How the agent review runs: `off`, `suggest`, `once-before-ship`, `before-ship`, or `after-turn` (see below) |
| `amp.brisk.askPerson` | `dialog` | When brisk asks for a person: `dialog` (label, Ship line, and the dialog before Ship), `note` (label and Ship line only), or `off` |
| `amp.brisk.artifact` | `true` | `false` stops the Brisk assessment artifact |
| `amp.brisk.unattended` | `stop` | When brisk asks for a person and nobody watches the thread: `stop` before pushing, or `ship` and leave the questions |

| Preset | `agentReview` | `confirmUnassessed` |
| --- | --- | --- |
| `light` | `once-before-ship` | off |
| `balanced` (default) | `once-before-ship` | off |
| `careful` | `before-ship` | on |

## Agent review

Every change with files gets one agent review. The plugin runs it by `agentReview`:

- `suggest`: shows a **Start agent review** quick action (in place of **Review risky changes**), which asks the agent to hand the changes to a reviewing subagent guided by brisk's plan.
- `once-before-ship`: also makes sure one review ran on this version of the changes before Ship. A review you started with the quick action, or one that started after a turn, counts; otherwise the Ship and Push to Branch turn runs it before pushing.
- `before-ship`: the Ship and Push to Branch turn always runs the review before pushing, even if one already ran.
- `after-turn`: also starts the review as soon as the agent's turn ends, once per version of the changes. A turn that was itself the review never starts another, a Ship turn that ran the review is not followed by another, and no review starts once you have sent a new message. If the review changed the code, Ship still asks for a review of the new version.
- `off`: none of this.

The review message follows brisk's review plan (`agent_review.plan`): one numbered section per area, in brisk's priority order, with the files and symbols it covers and the checks to make. The reviewer is asked for a verdict on every check: OK, fixed, or false alarm. Area and check text comes from brisk's catalog (`source: brisk`) or the project's base `.brisk.yaml` (`source: config`). A review that starts with no person involved (`after-turn`) uses brisk's areas only and says how many project areas it left out; the quick action and the Ship turn include them. Without a plan, the message lists brisk's reasons as before.

When the base `.brisk.yaml` names a `business_rules` file, brisk lists its path in `agent_review.context_files`, and every review message, unattended ones included, says "Read <path> for the project's business rules." Only the path goes into the message; the reviewing agent reads the file itself.

With the `amp` backend the plugin answers brisk's `review_questions` (one relevance score per area) with a separate `amp.ai.decide` call, limited to 5 s, and returns them as `review_answers`. With `review_when: always` (brisk's default) it asks them alongside the main call; with `needed` it asks them only after the main answers lead to `human_check.needed`, then runs `brisk assess --answers` again with them (if that second run fails, the first assessment stands). Jev only scores areas; brisk drops low-scoring areas that are not required and orders the rest. If the call fails, times out, or leaves areas unanswered, the plugin sends `review_error`, brisk keeps its full plan (`provenance.review_plan.status: failed`), and the assessment is not marked degraded. `amp.ai.decide` does not report token usage, so the plugin sends no `review_usage`.

The repository's own preset (in its base `.brisk.yaml`, reported by brisk as `provenance.preset`) can raise `agentReview`, never lower it, and cannot override `off`: `light` raises it to `once-before-ship`, `careful` to `before-ship`, and `balanced` (also what brisk reports when the file names no preset) raises nothing. The earlier preset names count the same. So a person who wants stricter review than the repository sets it in their own settings. Only your settings can choose `after-turn`, so a cloned repository cannot make the agent start turns by itself. Reasons in the review message quote the code, so each is collapsed to one line of at most 200 characters and marked as data.

When an assessment fails (for example brisk refuses answers for an earlier version of the change), the thread gets `brisk-risk-unknown` in place of its old risk label, the review action goes away, and the Ship prompt says the risk is unknown.

## Install

Copy or symlink this directory to `~/.config/amp/plugins/brisk/` (personal) or `.amp/plugins/brisk/` (one project), make sure `brisk` is on `PATH`, then run `plugins: reload`. The command palette also gets **brisk: Assess change risk**.

## Develop

```sh
bun install
bun test        # protocol, session record, and presentation, with a fake brisk
bunx tsc --noEmit
```

`amp plugins exec` does not run handlers; test inside a thread by loading the plugin with `load_plugin`.
