# Visual documentation

- When changes affect architecture or documented behavior, `docs/architecture.md`, its diagrams, and `docs/explainer/` must be updated before the change is pushed. This includes assessment flow, trust boundaries, backends, policy, agent review, harness integration, and evaluation.
- Do the docs and explainer updates in a separate thread wherever possible, so they do not block the code work. The code thread tells the docs thread what changed: the commits, new keys, new outputs, and new numbers. The docs thread regenerates and verifies the docs and explainer. The code thread pulls the docs thread's commit before it pushes.
- A code thread updates the docs itself only when the change is tiny or no separate thread is available.
- Keep the architecture overview high-level. Put contract details in the accompanying text and visual explainer.
- Edit diagram labels and routes in `docs/architecture.py`, then run `python3 docs/architecture.py`. Commit the generated SVG files with their source.
- When assessment examples change, update `docs/explainer/example.sh` and regenerate the embedded results with `docs/explainer/embed.py`. Use actual assessment outputs, not hand-edited results.
- Inspect the affected diagrams and explainer states in a browser. Verify the explainer's tier/floor self-check after changes to its examples or scoring presentation.

# Private evaluation repositories

- Never name the private repositories that the offline evaluation runs on, or quote identifying details from them: owner/repo paths, commit SHAs or subjects, file, package or type names, or their code. This applies to code, comments, tests, fixtures, recordings, docs and commit messages in this repository. Refer to them as repo A and repo B, or as the evaluation repositories.
- Their local paths live in the untracked `eval/repos.env` (see README). When a test or fixture needs a realistic case, adapt one from a popular public repository instead.
