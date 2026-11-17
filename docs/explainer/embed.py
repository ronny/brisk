#!/usr/bin/env python3
"""Embeds brisk's real outputs for the example changes into index.html.

usage: embed.py OUT_DIR   (OUT_DIR is what example.sh wrote)

CALIBRATION names the calibration table the page's levers use; it defaults
to calibration/jev-1.14.json in this repository.

Replaces the JSON between the EXAMPLES-DATA markers in index.html, so the
page stays one self-contained file.
"""
import json
import os
import pathlib
import re
import sys

out = pathlib.Path(sys.argv[1])
page = pathlib.Path(__file__).with_name("index.html")
cal_path = pathlib.Path(os.environ.get("CALIBRATION") or pathlib.Path(__file__).parents[2] / "calibration" / "jev-1.14.json")

CASES = [
    ("typo", "README typo", "docs: fix typo in README"),
    ("handler", "Removed access check in a Go handler", "web: let the edit handler save straight away"),
    ("refactor", "React component refactor", "web: extract DocumentRow from DocumentList"),
    ("migration", "Additive migration", "db: add archived_at to documents"),
    ("migration-drop", "Migration that drops a column", "db: drop documents.owner_id"),
    ("visibility", "New documents visible to the workspace", "docs: make new documents visible to the workspace"),
]

def project(f):
    if not f.exists():
        return None
    a = json.loads(f.read_text())
    return {k: a.get(k) for k in ("tier", "floor", "reasons", "agent_review")} | {
        "review_plan": a["provenance"].get("review_plan"),
        "rule_hits": a["provenance"]["rule_hits"],
        "answers": a["provenance"].get("answers", {}),
    }


examples = []
for key, title, message in CASES:
    host = json.loads((out / f"{key}.host.json").read_text())
    final = json.loads((out / f"{key}.final.json").read_text())
    session = json.loads((out / f"{key}.session.json").read_text())
    prov = final["provenance"]
    examples.append({
        "key": key,
        "title": title,
        "message": message,
        "session": session,
        "diff": (out / f"{key}.diff").read_text(),
        "state": host["state"],
        "request_id": host["request_id"],
        # Scored in a second Jev call; areas Jev rates below 1 are dropped unless required.
        "review_questions": host.get("review_questions"),
        "floor_assessment": {k: host["floor_assessment"][k] for k in ("tier", "floor", "reasons")},
        "final": {
            "tier": final["tier"],
            "floor": final["floor"],
            "impact": final["impact"],
            "uncertainty": final["uncertainty"],
            "action": final["action"],
            "agent_review": final.get("agent_review"),
            "reasons": final["reasons"],
            "stats": final["stats"],
            "provenance": {k: prov.get(k) for k in (
                "brisk_version", "backend", "model", "calibration", "diff_hash",
                "rule_hits", "findings", "model_risk", "duration_ms", "preset", "thresholds", "review_plan")}
            | {"answers": {q: {k: v for k, v in a.items() if k != "legend"} for q, a in prov["answers"].items()}},
        },
        "text": (out / f"{key}.final.txt").read_text(),
        # The same answers scored under each preset.
        "presets": {
            p: {k: a.get(k) for k in ("tier", "floor", "action", "agent_review")} | {"thresholds": a["provenance"].get("thresholds")}
            for p in ("autonomous", "reviewed", "strict")
            if (f := out / f"{key}.{p}.json").exists() and (a := json.loads(f.read_text()))
        },
        # Assessed with the project's own rules (project-rules.brisk.yaml).
        "project": project(out / f"{key}.project.json"),
    })

cal = json.loads(cal_path.read_text())
if len(cal.get("risk_quantiles") or []) != 101:
    sys.exit(f"{cal_path}: no risk_quantiles; the levers need a calibration with them (set CALIBRATION)")
data = json.dumps({"examples": examples, "questions": json.loads((out / "handler.host.json").read_text())["questions"],
                   "calibration": {k: cal.get(k) for k in ("id", "weights", "thresholds", "overrides", "risk_quantiles")},
                   "project_rules": (page.parent / "project-rules.brisk.yaml").read_text()},
                  indent=1, ensure_ascii=False).replace("</", "<\\/")
html = page.read_text()
html, n = re.subn(r"(<script id=\"examples-data\" type=\"application/json\">).*?(</script>)",
                  lambda m: m.group(1) + "\n" + data + "\n" + m.group(2), html, flags=re.S)
if n != 1:
    sys.exit("index.html: examples-data block not found")
page.write_text(html)
print(f"embedded {len(examples)} examples into {page}")
