# brisk for Amp

An Amp directory plugin that rates the risk of a thread's code changes with the `brisk` binary. It is advisory:

- After each agent turn that edits files or runs commands, it labels the thread `risk-low`, `risk-medium`, or `risk-high`, replacing the previous risk label.
- For Medium and High it shows a **Review risky changes** quick action that asks the agent to check each reason.
- For Medium and High it adds a line to the Ship and Push to Branch prompt, such as `Risk: High (Changes CanAccess). Before pushing, review …`.
- Optional, off by default and turned on per project in user settings: a confirmation before Ship or Push to Branch on a High-risk or unassessed change, which only a person can answer. Declining cancels the turn.

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
| `amp.brisk.preset` | `reviewed` | `autonomous`, `reviewed`, or `strict`. Sets the defaults of the two keys below |
| `amp.brisk.confirmShipOnHigh` | preset | `true` for every project, `false`, or a list of project roots, such as `["~/src/payments"]`. Before Ship or Push to Branch on a High-risk change, or when brisk could not assess the changes, a person must confirm; declining cancels the turn |
| `amp.brisk.agentReview` | preset | What to do when brisk recommends an agent review: `off`, `suggest`, `before-ship`, or `after-turn` (see below) |

| Preset | `agentReview` | `confirmShipOnHigh` | For |
| --- | --- | --- | --- |
| `autonomous` | `after-turn` | off | Nobody reviews the agent's work, so the agent reviews it itself |
| `reviewed` (default) | `suggest` | off | People review pull requests; brisk points them at the risky parts |
| `strict` | `before-ship` | on | Review before every risky Ship, and a person confirms High |

## Agent review

Separately from the tier, brisk says whether a further agent review is recommended, and why (`agent_review` in the assessment). The plugin then does, by `agentReview`:

- `suggest`: shows a **Start agent review** quick action (in place of **Review risky changes**), which asks the agent to hand the changes to a reviewing subagent pointed at brisk's reasons, and adds `brisk recommends an agent review: <reason>.` to the Ship prompt.
- `before-ship`: also tells the Ship and Push to Branch turn to run that review before pushing.
- `after-turn`: also starts the review as soon as the agent's turn ends, once per version of the changes. A turn that was itself the review never starts another, a Ship turn that ran the review is not followed by another, and no review starts once you have sent a new message. If the review changed the code, Ship still asks for a review of the new version.
- `off`: none of this.

The repository's own preset (in its base `.brisk.yaml`, reported by brisk as `provenance.preset`) can make `agentReview` more automatic than your setting, up to `before-ship`, never less, and cannot override `off`. Only your settings can choose `after-turn`, so a cloned repository cannot make the agent start turns by itself. Reasons in the review message quote the code, so each is collapsed to one line of at most 200 characters and marked as data. The binary and the Ship confirmation come only from your settings.

When an assessment fails (for example brisk refuses answers for an earlier version of the change), the thread gets `risk-unknown` in place of its old risk label, the review action goes away, and the Ship prompt says the risk is unknown.

## Install

Copy or symlink this directory to `~/.config/amp/plugins/brisk/` (personal) or `.amp/plugins/brisk/` (one project), make sure `brisk` is on `PATH`, then run `plugins: reload`. The command palette also gets **brisk: Assess change risk**.

## Develop

```sh
bun install
bun test        # protocol, session record, and presentation, with a fake brisk
bunx tsc --noEmit
```

`amp plugins exec` does not run handlers; test inside a thread by loading the plugin with `load_plugin`.
