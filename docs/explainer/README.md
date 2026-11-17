# brisk explainer

An interactive, diagram-first explainer of how brisk works. Open `index.html` in a browser; it is one self-contained file with no build step.

It walks six example changes to a repo with a Go backend and a React/TypeScript frontend through brisk, and covers the three flows (CLI and CI, the Amp plugin, offline evaluation and calibration), how "bad" is defined and measured, and the `.brisk.yaml` levers.

The example assessments are real brisk and Jev output. To regenerate them:

1. `BRISK=/path/to/brisk bash example.sh` builds the example repo in `/tmp/shelf` and writes host requests to `/tmp/shelf-out/req.jsonl`.
2. Answer them with Jev into `/tmp/shelf-out/ans.jsonl`. Requests keyed `<case>#review` carry the review plan's `review_questions`, a separate call on the same state. Use `brisk-eval answer -in /tmp/shelf-out/req.jsonl -out /tmp/shelf-out/ans.jsonl` (OpenRouter) or the `brisk_decide_batch` tool of the `eval/ampdecide` plugin (Amp).
3. `PHASE=2 BRISK=/path/to/brisk bash example.sh` scores the answers, also under each preset and with the project rules in `project-rules.brisk.yaml`, and embeds the results into `index.html` with `embed.py`.

`embed.py` reads the levers' calibration from `calibration/jev-1.14.json` and stops if it has no `risk_quantiles`; set `CALIBRATION` to use another table. Commit dates are fixed, so the example commits, and with them the request IDs, are the same on every run.

The page logs a mismatch and sets `data-selfcheck`, `data-presetcheck`, and `data-plancheck` on `<html>` if its tier, agent-review, or review-plan logic disagrees with brisk's embedded output. All three should read `ok`.
