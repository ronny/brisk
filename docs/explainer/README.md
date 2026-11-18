# brisk explainer

An interactive, diagram-first explainer of how brisk works. Open `index.html` in a browser; it is one self-contained file with no build step.

It shows how brisk decides whether a change needs a person (`human_check`), on top of the one review every change gets. It walks six example changes to a repo with a Go backend and a React/TypeScript frontend through brisk, and covers the three flows (CLI and CI, the Amp plugin, offline evaluation and calibration), how "bad" is defined and measured, and the `.brisk.yaml` levers.

The example assessments are real brisk and Jev output. To regenerate them:

1. `BRISK=/path/to/brisk bash example.sh` builds the example repo in `/tmp/shelf` and writes host requests to `/tmp/shelf-out/req.jsonl`.
2. Answer them with Jev into `/tmp/shelf-out/ans.jsonl`, with `brisk-eval answer -in /tmp/shelf-out/req.jsonl -out /tmp/shelf-out/ans.jsonl` (OpenRouter) or the `brisk_decide_batch` tool of the `eval/ampdecide` plugin (Amp). Both skip keys already answered.
3. `PHASE=2 BRISK=/path/to/brisk bash example.sh` scores the answers under no `.brisk.yaml`, each preset, and the project rules in `project-rules.brisk.yaml`. Like the Amp plugin, it also asks the review plan's questions: with the main ones by default (`review_when: always`), or only when a person is asked (`tailor_plan: needed`). The first time, it appends those requests (keys `<case>#review`) to `req.jsonl` and exits with status 4. Answer them as in step 2 and run phase 2 again; it then embeds the results into `index.html` with `embed.py`.

`embed.py` reads the levers' calibration from `calibration/jev-1.14.json` and stops if it has no `risk_quantiles`; set `CALIBRATION` to use another table. Commit dates are fixed, so the example commits, and with them the request IDs, are the same on every run.

The page logs a mismatch and sets `data-selfcheck`, `data-presetcheck`, and `data-plancheck` on `<html>` if its tier, `human_check`, or review-plan logic disagrees with brisk's embedded output. All three should read `ok`.
