package rules_test

import (
	"testing"

	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
	"github.com/ronny/brisk/rules"
)

func change(t *testing.T, path, added string) *risk.ChangeSet {
	t.Helper()
	raw := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1,0 +1,1 @@\n+" + added + "\n"
	files, err := gitx.ParseDiff([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return &risk.ChangeSet{Files: files}
}

func TestFloor(t *testing.T) {
	for _, tc := range []struct {
		name, path, line string
		want             risk.Tier
		hit              string
	}{
		// Tailwind's "truncate" class once fired destructive_sql.
		{"tailwind truncate", "frontend/src/routes/list.tsx", `"truncate font-medium",`, risk.Low, ""},
		{"drop column in migration", "db/migrations/004.sql", "ALTER TABLE users DROP COLUMN email;", risk.High, "destructive_sql"},
		{"delete without where in Go", "internal/store/s.go", `db.Exec("DELETE FROM sessions")`, risk.High, "destructive_sql"},
		{"delete with where", "internal/store/s.go", `db.Exec("DELETE FROM sessions WHERE id = $1", id)`, risk.Low, ""},
		{"destructive SQL only in a comment", "internal/store/s.go", "// we never DROP TABLE users", risk.Low, ""},
		{"tls off", "internal/client/c.go", "cfg := &tls.Config{InsecureSkipVerify: true}", risk.High, "tls_disabled"},
		{"aws key", "internal/upload/config.go", `const k = "AKIAZ7Q4EXAMPLE3KX2P"`, risk.High, "secret_added"},
		{"skip in a test", "internal/auth/access_test.go", `t.Skip("flaky")`, risk.Medium, "test_disabled"},
		{"auth path", "backend/internal/auth/token.go", "x := 1", risk.Medium, "auth"},
		{"auth path in a test is not auth", "backend/internal/auth/token_test.go", "x := 1", risk.Low, ""},
		{"package.json scripts only", "package.json", `"lint": "oxlint",`, risk.Low, ""},
		{"package.json dependency", "package.json", `"react": "^19.2.0",`, risk.Medium, "dependencies"},
		{"workflow", ".github/workflows/deploy.yml", "on: push", risk.Medium, "ci_deploy"},
		{"docs", "docs/auth.md", "DROP TABLE users", risk.Low, ""},
		// Lines that only start like a comment.
		{"PEM key in a Go raw string", "internal/keys/k.go", "-----BEGIN RSA PRIVATE KEY-----", risk.High, "secret_added"},
		{"key in a comment is still leaked", "internal/keys/k.go", "// AKIAZ7Q4EXAMPLE3KX2P", risk.High, "secret_added"},
		{"code after a closed block comment", "internal/client/c.go", "/**/ cfg := &tls.Config{InsecureSkipVerify: true}", risk.High, "tls_disabled"},
		{"pointer write", "internal/client/c.go", "*cfg = tls.Config{InsecureSkipVerify: true}", risk.High, "tls_disabled"},
		{"SQL comment in a migration", "db/migrations/004.sql", "-- DROP TABLE users later", risk.Medium, "migration"},
		{"down migration drops what up created", "db/migrations/0002_users.down.sql", "DROP TABLE users;", risk.Medium, "migration"},
		{"camelCase auth file", "src/authService.ts", "x()", risk.Medium, "auth"},
		{"camelCase permissions file", "internal/userPermissions.go", "x := 1", risk.Medium, "auth"},
		{"access log is not auth", "internal/access_log.go", "x := 1", risk.Low, ""},
		{"author is not auth", "internal/author.go", "x := 1", risk.Low, ""},
		{"brisk's own config", ".brisk.yaml", "sensitive_paths: []", risk.Medium, "brisk_config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := rules.Apply(change(t, tc.path, tc.line), nil, nil, rules.Config{})
			if got := rules.Floor(hits); got != tc.want {
				t.Fatalf("floor %s, want %s (hits %+v)", got, tc.want, hits)
			}
			if tc.hit != "" && (len(hits) == 0 || hits[0].ID != tc.hit) {
				t.Fatalf("hits %+v, want %s first", hits, tc.hit)
			}
		})
	}
}

func TestFindingsAndSessionAndConfig(t *testing.T) {
	cs := change(t, "billing2/x.go", "x := 1")
	hits := rules.Apply(cs, []risk.Finding{{Kind: risk.AuthLogicChanged, Path: "billing2/x.go", StartLine: 1}, {Kind: risk.ParseError, Path: "billing2/x.go"}}, nil, rules.Config{})
	if rules.Floor(hits) != risk.High || len(hits) != 1 || hits[0].Source != "language" {
		t.Fatalf("auth finding should set High and parse errors nothing: %+v", hits)
	}
	fail := 1
	hits = rules.Apply(cs, nil, &risk.Session{Commands: []risk.Command{{Command: "go test ./...", ExitCode: &fail}, {Command: "ls"}}}, rules.Config{})
	if rules.Floor(hits) != risk.Medium {
		t.Fatalf("a failed last check should set Medium: %+v", hits)
	}
	hits = rules.Apply(cs, nil, nil, rules.Config{PathRules: []risk.PathRule{{ID: "ledger", Paths: []string{"billing2/**"}, Floor: risk.High}}})
	if rules.Floor(hits) != risk.High || hits[0].ID != "config:ledger" {
		t.Fatalf(".brisk.yaml rule should set High: %+v", hits)
	}
}
