# brisk for Amp

An Amp directory plugin that rates the risk of a thread's code changes with the `brisk` binary. It is advisory:

- After each agent turn that edits files or runs commands, it labels the thread `risk-low`, `risk-medium`, or `risk-high`, replacing the previous risk label.
- For Medium and High it shows a **Review risky changes** quick action that asks the agent to check each reason.
- For Medium and High it adds a line to the Ship and Push to Branch prompt, such as `Risk: High (Changes CanAccess). Before pushing, review …`.
- Optional, off by default: a confirmation before Ship or Push to Branch on a High-risk change, which only a person can answer. Declining cancels the turn.

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

Set these in Amp settings. To apply one to a single project, put it in that repository's `.amp/settings.json` with an `amp.` prefix, for example `{ "amp.brisk.confirmShipOnHigh": true }`.

| Key | Default | |
| --- | --- | --- |
| `brisk.binary` | `brisk` on `PATH` | Path to the brisk binary |
| `brisk.backend` | `auto` | See above |
| `brisk.base` | brisk's default | Base ref to compare against |
| `brisk.confirmShipOnHigh` | `false` | Ask a person to confirm Ship or Push to Branch on High risk |

## Install

Copy or symlink this directory to `~/.config/amp/plugins/brisk/` (personal) or `.amp/plugins/brisk/` (one project), make sure `brisk` is on `PATH`, then run `plugins: reload`. The command palette also gets **brisk: Assess change risk**.

## Develop

```sh
bun install
bun test        # protocol, session record, and presentation, with a fake brisk
bunx tsc --noEmit
```

`amp plugins exec` does not run handlers; test inside a thread by loading the plugin with `load_plugin`.
