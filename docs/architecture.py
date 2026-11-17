"""Render the editable architecture diagrams with Python's standard library.

Run from any directory: python3 docs/architecture.py
Coordinates, labels, and routes below are the diagram source. SVG files are
also editable in vector editors. No network, fonts download, or packages needed.
"""

from html import escape
from pathlib import Path

ROOT = Path(__file__).resolve().parent
INK, MUTED, BORDER, BLUE = "#17243b", "#526078", "#aab5c5", "#245bcb"


class Diagram:
    def __init__(self, title, subtitle, height):
        self.parts = [f'''<svg xmlns="http://www.w3.org/2000/svg"
  width="1200" height="{height}" viewBox="0 0 1200 {height}"
  role="img" aria-labelledby="title description">
<title id="title">{escape(title)}</title>
<desc id="description">{escape(subtitle)}</desc>
<defs>
  <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5"
    markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="{MUTED}"/>
  </marker>
  <marker id="blue-arrow" viewBox="0 0 10 10" refX="9" refY="5"
    markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="{BLUE}"/>
  </marker>
</defs>
<rect width="1200" height="{height}" fill="white"/>
<g font-family="DejaVu Sans, sans-serif" fill="{INK}">''']
        self.text(40, 48, title, 25, weight=600)
        self.text(40, 79, subtitle, 15, MUTED)

    def text(self, x, y, value, size=14, color=INK, weight=400, anchor="start"):
        self.parts.append(
            f'<text x="{x}" y="{y}" font-size="{size}" fill="{color}" '
            f'font-weight="{weight}" text-anchor="{anchor}">{escape(value)}</text>'
        )

    def box(self, x, y, width, height, title, lines, *, blue=False, future=False):
        color = BLUE if blue else BORDER
        dash = ' stroke-dasharray="6 5"' if future else ""
        self.parts.append(
            f'<rect x="{x}" y="{y}" width="{width}" height="{height}" '
            f'rx="8" fill="white" stroke="{color}" '
            f'stroke-width="{1.7 if blue else 1}"{dash}/>'
        )
        self.text(x + 18, y + 29, title, 16, weight=600)
        for i, line in enumerate(lines):
            self.text(x + 18, y + 57 + i * 22, line, 13, MUTED)

    def edge(self, points, label="", at=None, *, blue=False, dashed=False, both=False):
        color = BLUE if blue else MUTED
        marker = "blue-arrow" if blue else "arrow"
        dash = ' stroke-dasharray="6 5"' if dashed else ""
        start = f' marker-start="url(#{marker})"' if both else ""
        self.parts.append(
            f'<polyline points="{points}" fill="none" stroke="{color}" '
            f'stroke-width="{1.7 if blue else 1.2}" '
            f'marker-end="url(#{marker})"{start}{dash}/>'
        )
        if label:
            x, y = at
            # A white halo keeps labels clear where a route passes nearby.
            self.parts.append(f'<g paint-order="stroke" stroke="white" stroke-width="5">')
            self.text(x, y, label, 12, color, anchor="middle")
            self.parts.append("</g>")

    def save(self, name):
        (ROOT / name).write_text("\n".join(self.parts + ["</g></svg>"]) + "\n")


def runtime():
    d = Diagram("Brisk rates the risk of a code change",
                "One independent core. Rules set a floor that the model cannot lower.", 735)

    d.box(400, 125, 400, 95, "Harness adapters", [
        "Amp captures session context and shows results.",
        "Other harnesses: interface-only"])
    d.box(40, 295, 300, 145, "Inputs · untrusted", [
        "Diff against a base commit",
        "Optional session record",
        "Code and transcripts are evidence,",
        "not instructions or policy."])
    d.box(400, 295, 400, 145, "Brisk core library + CLI", [
        "Combines rules, code analysis and model answers",
        "Produces one evidence-backed assessment",
        "Model can raise risk, never lower the rule floor.",
        "Works without a harness or a model."], blue=True)
    d.box(860, 295, 300, 145, "Assessment", [
        "Low / Medium / High",
        "Reasons, evidence and next action",
        "Human check: ask a person? + what to ask",
        "Provenance records the applied policy"], blue=True)

    d.edge("600,220 600,295", "calls CLI + supplies session", (700, 263))
    d.edge("340,365 400,365", "input", (370, 352))
    d.edge("800,365 860,365", "result", (830, 352), blue=True)

    d.box(40, 555, 330, 120, "Language analysis", [
        "Go and TypeScript / React",
        "Tree-sitter WASM on wazero",
        "Reports structural changes"])
    d.box(435, 555, 330, 120, "Project context + policy", [
        "Git history; path + content rules",
        "Base config: policy, questions, rules file",
        "Presets: light / balanced / careful"])
    d.box(830, 555, 330, 120, "Classifier backends", [
        "Jev through OpenRouter",
        "Host: harness answers the questions",
        "Rules-only: no model"])
    d.edge("205,555 205,495 480,495 480,440", "findings", (298, 485))
    d.edge("600,555 600,440", "context", (634, 522))
    d.edge("995,555 995,495 720,495 720,440", "answers", (911, 485))
    d.text(40, 715, "MVP components shown · balanced is the default preset · Additional implementations are interface-only", 12, MUTED)
    d.save("architecture.svg")


def evaluation():
    d = Diagram("CI replays recorded answers through the same scoring core",
                "Live model access is an explicit recording step, not part of ordinary CI.", 1070)
    d.text(40, 108, "Blue: deterministic replay     Dashed routes: explicit recording or calibration feedback", 12, MUTED)
    d.box(40, 145, 520, 128, "Historical changes · offline evaluation", [
        "Repo A + repo B (private) · brisk-eval label / prepare",
        "Reverts, fixes within 14 days, optional supplied CI failures",
        "Noisy outcome labels, not expert ground truth"])
    d.box(640, 145, 520, 128, "Fixtures + injection variants · CI", [
        "testdata/fixtures/ + testdata/injection/",
        "Expected tiers and rule-floor assertions",
        "Normal tests do not ask the live model"], blue=True)
    d.box(340, 335, 520, 105, "Shared core · Engine.Prepare", [
        "Same diff collection, rules, context and language backends",
        "Produces prepared evidence, state, typed questions and a key"])
    d.edge("300,273 300,300 490,300 490,335", "prepare history", (391, 291))
    d.edge("900,273 900,300 710,300 710,335", "prepare fixtures", (805, 291), blue=True)

    d.box(40, 520, 330, 150, "Explicit recording step", [
        "OpenRouter → Jev OR eval/ampdecide",
        "Record new / changed requests",
        "Manual refresh workflow uploads",
        "answers for review and commit"])
    d.box(435, 520, 330, 150, "Recorded answers · JSONL", [
        "Request key, exact model, typed answers",
        "Fixtures: testdata/answers/*.jsonl",
        "Historical answers: separate files",
        "No credentials in recordings"], blue=True)
    d.box(830, 520, 330, 150, "Deterministic replay", [
        "Match request key to recorded answers",
        "Missing fixture recording fails the test",
        "No API key, network call or model cost",
        "Key distinct from host protocol ID"], blue=True)
    d.edge("400,440 400,480 205,480 205,520", "new / changed requests", (258, 468), dashed=True)
    d.edge("370,595 435,595", "save", (401, 583), dashed=True)
    d.edge("765,595 830,595", "load", (797, 583), blue=True)
    d.edge("800,440 800,480 995,480 995,520", "key + prepared evidence", (991, 468), blue=True)

    d.box(435, 742, 725, 105, "Shared scoring logic · assess/ + calibration/", [
        "Prepared.Finish for fixtures; reused scoring functions for historical records",
        "Model-version calibration + rule floor · same tiering contract as the CLI"], blue=True)
    d.edge("995,670 995,742", "recorded typed answers", (1090, 711), blue=True)
    d.box(40, 742, 330, 105, "Versioned calibration data", [
        "Fit on training data; review the change",
        "Never a runtime self-update"])
    d.edge("370,794 435,794", "data", (402, 782))
    d.box(435, 905, 330, 95, "GitHub Actions · CI", [
        "Fixture replay, vet, tests, static builds",
        "Amp plugin tests + TypeScript check"], blue=True)
    d.box(830, 905, 330, 95, "Offline evaluation report", [
        "Held-out metrics + baselines, stability,",
        "latency, cost and per-preset results"])
    d.edge("600,847 600,905", "assertions", (648, 885), blue=True)
    d.edge("995,847 995,905", "scores + labels", (1074, 885))
    d.edge("830,957 800,957 800,1047 205,1047 205,847", "fit feedback from training split only", (464, 1038), dashed=True)
    d.save("architecture-evaluation.svg")


if __name__ == "__main__":
    runtime()
    evaluation()
