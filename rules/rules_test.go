package rules_test

import (
	"strings"
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

func protoChange(t *testing.T, old, new string) *risk.ChangeSet {
	t.Helper()
	// Rules read whole files for protobuf; the hunk only marks the file
	// as changed.
	raw := "diff --git a/api.proto b/api.proto\n--- a/api.proto\n+++ b/api.proto\n@@ -1 +1 @@\n-x\n+y\n"
	files, err := gitx.ParseDiff([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	files[0].Old, files[0].New = []byte(old), []byte(new)
	return &risk.ChangeSet{Files: files}
}

func TestProtoRenumbered(t *testing.T) {
	const base = `syntax = "proto3";
message Order {
  string id = 1;
  int64 total = 2; // cents
  string customer = 3;
  oneof pay {
    string card = 4;
  }
  message Line {
    string sku = 1;
    int32 qty = 2;
  }
}
message Supplier
{
  string code = 3 [(buf.validate.field).cel = {
    id: "code"
  }];
  string id = 1;
  string name = 2 [(buf.validate.field).required = true];
}
`
	for _, tc := range []struct {
		name, from, to string
		want           bool
	}{
		{"swap two numbers", "int64 total = 2; // cents\n  string customer = 3;", "string customer = 2;\n  int64 total = 3; // cents", true},
		{"add a field", "string customer = 3;", "string customer = 3;\n  string note = 5;", false},
		{"rename keeps number and type", "string customer = 3;", "string customer_id = 3;", false},
		{"reuse a number with another type", "string customer = 3;", "int64 customer_count = 3;", true},
		{"change a type incompatibly", "string customer = 3;", "int64 customer = 3;", true},
		{"widen int32 to int64", "int32 qty = 2;", "int64 qty = 2;", false},
		{"braces and slashes in strings and comments", "string id = 1;\n  int64 total = 2; // cents", "string id = 1 [(x).doc = \"https://e.com/{id}\"]; /* { */\n  int64 total = 2; // cents", false},
		{"renumber after an option with a URL", "string id = 1;\n  int64 total = 2; // cents", "option (x) = { doc: \"https://e.com\" };\n  string id = 1;\n  int64 total = 9; // cents", true},
		{"renumber inside a oneof", "string card = 4;", "string card = 6;", true},
		{"renumber in a nested message", "int32 qty = 2;", "int32 qty = 3;", true},
		{"same name, other message, other number", "string name = 2 [(buf.validate.field).required = true];", "string name = 2 [\n    (buf.validate.field).required = true\n  ];", false},
		{"renumber after multi-line options", "string name = 2 [(buf.validate.field).required = true];", "string name = 5 [(buf.validate.field).required = true];", true},
		{"remove a whole message", "message Supplier\n{\n  string code = 3 [(buf.validate.field).cel = {\n    id: \"code\"\n  }];\n  string id = 1;\n  string name = 2 [(buf.validate.field).required = true];\n}\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			to := strings.Replace(base, tc.from, tc.to, 1)
			if to == base {
				t.Fatal("bad test: replacement did nothing")
			}
			hits := rules.Apply(protoChange(t, base, to), nil, nil, rules.Config{})
			got := false
			for _, h := range hits {
				if h.ID == "proto_renumbered" {
					got = h.Tier == risk.High
				}
			}
			if got != tc.want {
				t.Fatalf("proto_renumbered %v, want %v (hits %+v)", got, tc.want, hits)
			}
		})
	}
}
