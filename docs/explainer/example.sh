#!/usr/bin/env bash
# Builds the explainer's example repo "shelf" (Go backend, React/TS frontend),
# makes five example changes on branches, and runs brisk on each.
#
#   1. bash example.sh                       # host requests -> $OUT/req.jsonl
#   2. answer $OUT/req.jsonl into $OUT/ans.jsonl with Jev, either
#        brisk-eval answer -in $OUT/req.jsonl -out $OUT/ans.jsonl   (OpenRouter)
#      or the eval/ampdecide plugin's brisk_decide_batch tool (Amp)
#   3. PHASE=2 bash example.sh               # final assessments, embedded into index.html
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# Fixed dates give the same commit SHAs, and so the same request IDs, on every run.
export GIT_AUTHOR_DATE="2026-11-17T09:00:00+11:00" GIT_COMMITTER_DATE="2026-11-17T09:00:00+11:00"
R=${R:-/tmp/shelf}
OUT=${OUT:-/tmp/shelf-out}
BRISK=${BRISK:-/tmp/brisk}
if [ "${PHASE:-1}" = 1 ]; then
rm -rf "$R" "$OUT"; mkdir -p "$R" "$OUT"; cd "$R"
git init -q -b main
git config user.email dev@example.com; git config user.name Dev

mkdir -p backend/internal/web backend/internal/db/migrations backend/internal/docs frontend/src/components
cat > README.md <<'EOF'
# Shelf

Shelf stores and shares documents. The Go API lives in `backend/`, and the React app in `frontend/`.

## Develop

Run `make dev` to start the API and the web app togther.
EOF
cat > backend/go.mod <<'EOF'
module example.com/shelf

go 1.26
EOF
cat > backend/internal/web/documents.go <<'EOF'
package web

import "net/http"

// EditDocument saves a new body for a document the user may edit.
func (s *Server) EditDocument(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	doc, err := s.store.Document(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if doc.OwnerID != user.ID && !user.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.store.Save(doc.ID, r.FormValue("body")); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
EOF
cat > backend/internal/db/migrations/0006_documents.up.sql <<'EOF'
CREATE TABLE documents (
  id uuid PRIMARY KEY,
  owner_id uuid NOT NULL,
  body text NOT NULL
);
EOF
cat > frontend/package.json <<'EOF'
{
  "name": "shelf-web",
  "private": true,
  "scripts": { "dev": "vite", "test": "vitest" },
  "dependencies": { "react": "^19.2.0" }
}
EOF
cat > frontend/src/components/DocumentList.tsx <<'EOF'
import type { Doc } from "../api";

export function DocumentList({ docs, onOpen }: { docs: Doc[]; onOpen: (id: string) => void }) {
  return (
    <ul className="docs">
      {docs.map((d) => (
        <li key={d.id} className="doc">
          <button onClick={() => onOpen(d.id)}>{d.title}</button>
          <span className="owner">{d.ownerName}</span>
        </li>
      ))}
    </ul>
  );
}
EOF
cat > backend/internal/docs/visibility.go <<'EOF'
package docs

// DefaultVisibility is who can see a new document until its owner shares it.
const DefaultVisibility = "private"
EOF
git add -A; git commit -qm "shelf: initial"

# 1. README typo
git checkout -qb typo main
sed -i 's/togther/together/' README.md
git commit -qam "docs: fix typo in README"

# 2. Removed access check in a Go handler
git checkout -qb handler main
python3 - <<'EOF'
p="backend/internal/web/documents.go"
s=open(p).read()
s=s.replace("""	user := s.currentUser(r)
""","").replace("""	if doc.OwnerID != user.ID && !user.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
""","")
open(p,"w").write(s)
EOF
git commit -qam "web: let the edit handler save straight away"

# 3. React component refactor
git checkout -qb refactor main
cat > frontend/src/components/DocumentList.tsx <<'EOF'
import type { Doc } from "../api";

type Props = { docs: Doc[]; onOpen: (id: string) => void };

function DocumentRow({ doc, onOpen }: { doc: Doc; onOpen: (id: string) => void }) {
  return (
    <li className="doc">
      <button onClick={() => onOpen(doc.id)}>{doc.title}</button>
      <span className="owner">{doc.ownerName}</span>
    </li>
  );
}

export function DocumentList({ docs, onOpen }: Props) {
  return (
    <ul className="docs">
      {docs.map((d) => (
        <DocumentRow key={d.id} doc={d} onOpen={onOpen} />
      ))}
    </ul>
  );
}
EOF
git commit -qam "web: extract DocumentRow from DocumentList"

# 4. Migration, additive
git checkout -qb migration main
cat > backend/internal/db/migrations/0007_archive_documents.up.sql <<'EOF'
ALTER TABLE documents ADD COLUMN archived_at timestamptz;
CREATE INDEX documents_archived_at ON documents (archived_at);
EOF
cat > backend/internal/db/migrations/0007_archive_documents.down.sql <<'EOF'
DROP INDEX documents_archived_at;
ALTER TABLE documents DROP COLUMN archived_at;
EOF
git add -A; git commit -qm "db: add archived_at to documents"

# 5. Migration that drops a column
git checkout -qb migration-drop main
cat > backend/internal/db/migrations/0007_drop_owner.up.sql <<'EOF'
ALTER TABLE documents DROP COLUMN owner_id;
EOF
git add -A; git commit -qm "db: drop documents.owner_id"

# 6. A business-rule change no built-in rule knows about
git checkout -qb visibility main
sed -i 's/= "private"/= "workspace"/' backend/internal/docs/visibility.go
git commit -qam "docs: make new documents visible to the workspace"
git checkout -q main
fi

session() { # intent, command, exit
  cat <<EOF
{"schema":"brisk.session/v1","harness":"amp","session_id":"T-example","intent":"$1","commands":[{"command":"$2","exit_code":$3}]}
EOF
}
session "Fix the typo in the README" "git diff" 0 > "$OUT/typo.session.json"
session "Make saving faster in the edit handler" "go build ./..." 0 > "$OUT/handler.session.json"
session "Split DocumentList into smaller components" "pnpm test" 0 > "$OUT/refactor.session.json"
session "Add archiving to documents" "go test ./..." 0 > "$OUT/migration.session.json"
session "Remove the unused owner column" "go test ./..." 0 > "$OUT/migration-drop.session.json"
session "Make new documents easier to share" "go test ./..." 0 > "$OUT/visibility.session.json"

CASES="typo handler refactor migration migration-drop visibility"
# PROJECT is shelf's own .brisk.yaml with project rules; each change is also
# assessed with it. Its question changes the request, so it gets its own answers.
PROJECT="$HERE/project-rules.brisk.yaml"
# Phase 1 (default): host requests and rules-only. Phase 2 (PHASE=2): final
# assessments from answers written by the brisk_decide_batch plugin tool.
#
# Every change gets an agent review, so by default (review_when: always) the
# review plan's questions are answered with the main ones. Phase 2 appends
# those requests (key <case>#review) to req.jsonl and stops; answer them and
# run phase 2 again.
if [ "${PHASE:-1}" = 1 ]; then
  : > "$OUT/req.jsonl"
  for c in $CASES; do
    common=(assess --repo "$R" --base main --head "$c" --session "$OUT/$c.session.json" --format json)
    "$BRISK" "${common[@]}" --backend host > "$OUT/$c.host.json"
    "$BRISK" "${common[@]}" --backend rules-only > "$OUT/$c.rules.json"
    "$BRISK" "${common[@]}" --config "$PROJECT" --backend host > "$OUT/$c.project.host.json"
    git -C "$R" diff main "$c" > "$OUT/$c.diff"
    jq -c --arg k "$c" '{key:$k, request_id, state, questions}' "$OUT/$c.host.json" >> "$OUT/req.jsonl"
    jq -c --arg k "$c@project" '{key:$k, request_id, state, questions}' "$OUT/$c.project.host.json" >> "$OUT/req.jsonl"
  done
else
  # The last successful answer per key; a retried request leaves an error line too.
  # With a second argument, the review answers for that key are added.
  answers() {
    jq -s --arg k "$1" --arg r "${2:-}" '
      def last_ok($key): map(select(.key==$key and (.error|not)))|last;
      last_ok($k) as $m | (if $r == "" then null else last_ok($r) end) as $ra
      | {schema:"brisk.host-answers/v1", request_id:$m.request_id, model:$m.model, answers:$m.answers}
        + (if $ra then {review_answers:$ra.answers} else {} end)' "$OUT/ans.jsonl"
  }
  has_answer() { jq -e -s --arg k "$1" 'any(.[]; .key==$k and (.error|not))' "$OUT/ans.jsonl" >/dev/null; }
  pending=0
  # score OUTFILE KEY HOSTFILE [brisk flags...]: score with the main answers
  # and, when the request carries review questions (review_when: always, the
  # default, or needed once human_check asks for a person), the review answers too.
  score() {
    local outf=$1 key=$2 host=$3; shift 3
    answers "$key" > "$OUT/tmp.answers.json"
    "$BRISK" "${common[@]}" "$@" --answers "$OUT/tmp.answers.json" --format json > "$outf"
    if jq -e '.review_questions' "$host" >/dev/null && { jq -e '.review_when == "always"' "$host" >/dev/null || jq -e '.human_check.needed' "$outf" >/dev/null; }; then
      if has_answer "$key#review"; then
        answers "$key" "$key#review" > "$OUT/tmp.answers.json"
        "$BRISK" "${common[@]}" "$@" --answers "$OUT/tmp.answers.json" --format json > "$outf"
      elif ! grep -q "\"key\":\"$key#review\"" "$OUT/req.jsonl"; then
        jq -c --arg k "$key#review" '{key:$k, request_id, state, questions: .review_questions}' "$host" >> "$OUT/req.jsonl"
        pending=1
      else
        pending=1
      fi
    fi
    cp "$OUT/tmp.answers.json" "${outf%.json}.answers-used.json"
  }
  for p in light balanced careful; do printf 'preset: %s\n' "$p" > "$OUT/$p.brisk.yaml"; done
  for c in $CASES; do
    common=(assess --repo "$R" --base main --head "$c" --session "$OUT/$c.session.json")
    score "$OUT/$c.final.json" "$c" "$OUT/$c.host.json"
    "$BRISK" "${common[@]}" --answers "$OUT/$c.final.answers-used.json" --format text > "$OUT/$c.final.txt"
    # The same answers under each preset, as a repo's base .brisk.yaml would set it.
    for p in light balanced careful; do
      score "$OUT/$c.$p.json" "$c" "$OUT/$c.host.json" --config "$OUT/$p.brisk.yaml"
    done
    score "$OUT/$c.project.json" "$c@project" "$OUT/$c.project.host.json" --config "$PROJECT"
  done
  rm -f "$OUT/tmp.answers.json"
  if [ "$pending" = 1 ]; then
    echo "Review questions added to $OUT/req.jsonl: answer them, then run PHASE=2 again." >&2
    exit 4
  fi
  python3 "$HERE/embed.py" "$OUT"
fi
