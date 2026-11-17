# Visual documentation

- When changes affect architecture or documented behavior, update `docs/architecture.md`, its diagrams, and `docs/explainer/index.html` in the same change. This includes assessment flow, trust boundaries, backends, policy, agent review, harness integration, and evaluation.
- Keep the architecture overview high-level. Put contract details in the accompanying text and visual explainer.
- Edit diagram labels and routes in `docs/architecture.py`, then run `python3 docs/architecture.py`. Commit the generated SVG files with their source.
- When assessment examples change, update `docs/explainer/example.sh` and regenerate the embedded results with `docs/explainer/embed.py`. Use actual assessment outputs, not hand-edited results.
- Inspect the affected diagrams and explainer states in a browser. Verify the explainer's tier/floor self-check after changes to its examples or scoring presentation.
