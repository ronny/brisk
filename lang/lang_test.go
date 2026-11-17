package lang_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ronny/brisk/lang"
	"github.com/ronny/brisk/risk"
)

// change builds a FileChange from old and new content, with hunks from a
// line diff, as the engine would.
func change(path, old, new string) risk.FileChange {
	fc := risk.FileChange{Path: path, Status: risk.Modified, Old: []byte(old), New: []byte(new)}
	switch {
	case old == "":
		fc.Status = risk.Added
	case new == "":
		fc.Status = risk.Deleted
	}
	a, b := lines(old), lines(new)
	// LCS table, fine for test-sized inputs.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var h *risk.Hunk
	flush := func() {
		if h != nil {
			if h.NewLines == 0 {
				h.NewStart-- // git's convention: the line before a pure deletion
			}
			fc.Hunks = append(fc.Hunks, *h)
			h = nil
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			flush()
			i, j = i+1, j+1
			continue
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			// Deletions first, as git prints them.
			if h == nil {
				h = &risk.Hunk{OldStart: i + 1, NewStart: j + 1}
			}
			h.OldLines++
			h.Lines = append(h.Lines, "-"+a[i])
			fc.Deletions++
			i++
		default:
			if h == nil {
				h = &risk.Hunk{OldStart: i + 1, NewStart: j + 1}
			}
			h.NewLines++
			h.Lines = append(h.Lines, "+"+b[j])
			fc.Additions++
			j++
		}
	}
	flush()
	return fc
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func analyze(t *testing.T, fc risk.FileChange) []risk.Finding {
	t.Helper()
	for _, b := range lang.Backends() {
		if b.Supports(fc.Path) {
			fs, err := b.Analyze(context.Background(), fc)
			if err != nil {
				t.Fatalf("Analyze(%s): %v", fc.Path, err)
			}
			return fs
		}
	}
	t.Fatalf("no backend supports %s", fc.Path)
	return nil
}

// summary renders findings as "kind symbol @line" for comparison.
func summary(fs []risk.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s %s @%d", f.Kind, f.Symbol, f.StartLine))
	}
	slices.Sort(out)
	return out
}

type tc struct {
	name, path, old, new string
	want                 []string // "kind symbol @line"; nil means no findings
}

func run(t *testing.T, cases []tc) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := analyze(t, change(c.path, c.old, c.new))
			got := summary(fs)
			want := slices.Clone(c.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("findings\n got: %q\nwant: %q", got, want)
				for _, f := range fs {
					t.Logf("  %+v", f)
				}
			}
			for _, f := range fs {
				if f.Detail == "" || f.Path != c.path || f.Language == "" {
					t.Errorf("incomplete finding %+v", f)
				}
			}
		})
	}
}

func TestSupports(t *testing.T) {
	want := map[string]string{
		"a/b.go": "go", "x.ts": "typescript", "x.tsx": "typescript", "x.jsx": "typescript",
		"x.mjs": "typescript", "x.min.js": "", "README.md": "", "go.mod": "", "x.d.ts": "typescript",
	}
	for p, w := range want {
		got := ""
		for _, b := range lang.Backends() {
			if b.Supports(p) {
				got = b.Name()
			}
		}
		if got != w {
			t.Errorf("Supports(%q): got backend %q, want %q", p, got, w)
		}
	}
}

func TestGo(t *testing.T) {
	const api = `package api

type Server struct {
	Addr string
	mu   int
}

// Get returns a thing.
func Get(id int) error {
	return nil
}

func (s *Server) Handle(path string) {}

func helper() {}
`
	run(t, []tc{
		{"param type changes signature", "api/api.go", api,
			strings.Replace(api, "func Get(id int) error", "func Get(id string) error", 1),
			[]string{"exported_signature_changed Get @9"}},
		{"body and comment edits do not", "api/api.go", api,
			strings.Replace(strings.Replace(api, "return nil", "return Other()", 1), "// Get returns a thing.", "// Get returns a thing, better.", 1),
			nil},
		{"reformatting does not", "api/api.go", api,
			strings.Replace(api, "func Get(id int) error {", "func Get(id int)   error {", 1),
			nil},
		{"unexported field does not", "api/api.go", api,
			strings.Replace(api, "\tmu   int\n", "\tmu   int\n\tn    int\n", 1),
			nil},
		{"exported field does", "api/api.go", api,
			strings.Replace(api, "\tmu   int\n", "\tmu   int\n\tName string\n", 1),
			[]string{"exported_signature_changed Server @3"}},
		{"removed method", "api/api.go", api,
			strings.Replace(api, "func (s *Server) Handle(path string) {}\n", "", 1),
			[]string{"exported_symbol_removed (*Server).Handle @13"}},
		{"generic constraint changes signature", "api/api.go",
			api + "\ntype Box[T any] struct{ V T }\n",
			api + "\ntype Box[T comparable] struct{ V T }\n",
			[]string{"exported_signature_changed Box @17"}},
		{"unexported function removal is not API", "api/api.go", api,
			strings.Replace(api, "func helper() {}\n", "", 1),
			nil},
		{"deleted file removes API", "api/api.go", api, "",
			[]string{"exported_symbol_removed (*Server).Handle @13", "exported_symbol_removed Get @9", "exported_symbol_removed Server @3"}},
	})

	const auth = `package user

func CanAccess(u User, doc Doc) bool {
	if u.ID != doc.Owner {
		return false
	}
	return true
}

func canonical(s string) string {
	if s == "" {
		return "x"
	}
	return s
}

func Controller() int {
	return 1
}
`
	run(t, []tc{
		{"auth condition changed", "user/user.go", auth,
			strings.Replace(auth, "if u.ID != doc.Owner {", "if u.ID != doc.Owner && !u.Admin {", 1),
			[]string{"auth_logic_changed CanAccess @4"}},
		{"auth check removed", "user/user.go", auth,
			strings.Replace(auth, "\tif u.ID != doc.Owner {\n\t\treturn false\n\t}\n", "", 1),
			[]string{"auth_logic_changed CanAccess @4"}},
		{"names that only contain can/role/acl are not auth", "user/user.go", auth,
			strings.Replace(strings.Replace(auth, `return "x"`, `return "y"`, 1), "return 1", "return 2", 1),
			nil},
		{"any function in an auth path", "internal/auth/check.go", auth,
			strings.Replace(auth, `return "x"`, `return "y"`, 1),
			[]string{"auth_logic_changed canonical @12"}},
		{"generated header added to a hand-written file", "user/user.go", auth,
			"// Code generated by hand. DO NOT EDIT.\n" + strings.Replace(auth, "\tif u.ID != doc.Owner {\n\t\treturn false\n\t}\n", "", 1),
			[]string{"auth_logic_changed CanAccess @4"}},
		{"new file with a generated header is analyzed", "internal/auth/hooks_gen.go", "",
			"// Code generated by hand. DO NOT EDIT.\npackage auth\n\nfunc init() {\n\tbypass = func() bool { return true }\n}\n",
			[]string{"auth_logic_changed init @5"}},
	})

	const guards = `package doc

func Read(isOwner bool) bool {
	if !isOwner {
		return false
	}
	return true
}

func Write(isOwner bool) bool {
	if !isOwner {
		return false
	}
	return true
}

func Both(isOwner bool) bool {
	if !isOwner {
		return false
	}
	log()
	if !isOwner {
		return false
	}
	return true
}
`
	run(t, []tc{
		{"guard removed while an identical one survives in another function", "doc/doc.go", guards,
			strings.Replace(guards, "func Read(isOwner bool) bool {\n\tif !isOwner {\n\t\treturn false\n\t}\n", "func Read(isOwner bool) bool {\n", 1),
			[]string{"auth_logic_changed Read @4"}},
		{"first of two identical guards removed", "doc/doc.go", guards,
			strings.Replace(guards, "func Both(isOwner bool) bool {\n\tif !isOwner {\n\t\treturn false\n\t}\n", "func Both(isOwner bool) bool {\n", 1),
			[]string{"auth_logic_changed Both @18"}},
		{"second of two identical guards removed", "doc/doc.go", guards,
			strings.Replace(guards, "\tlog()\n\tif !isOwner {\n\t\treturn false\n\t}\n", "\tlog()\n", 1),
			[]string{"auth_logic_changed Both @22"}},
		{"guard moved within its function is kept", "doc/doc.go", guards,
			strings.Replace(guards, "func Read(isOwner bool) bool {\n\tif !isOwner {\n\t\treturn false\n\t}\n\treturn true\n", "func Read(isOwner bool) bool {\n\tprepare()\n\tif !isOwner {\n\t\treturn false\n\t}\n\treturn true\n", 1),
			nil},
	})

	const handler = `package web

import "net/http"

func (s *Server) editDocument(w http.ResponseWriter, r *http.Request) {
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
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if doc.Title == "" || user.Name == "" {
		return
	}
	if !s.limiter.Allow() {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	s.store.Save(doc.ID, r.FormValue("body"))
}
`
	run(t, []tc{
		// Thread A's testdata/injection/handler-permission.txt.
		{"ownership check removed from a handler", "internal/web/documents.go", handler,
			strings.Replace(strings.Replace(handler, "\tuser := s.currentUser(r)\n", "", 1),
				"\tif doc.OwnerID != user.ID && !user.IsAdmin {\n\t\thttp.Error(w, \"forbidden\", http.StatusForbidden)\n\t\treturn\n\t}\n", "", 1),
			[]string{"auth_logic_changed (*Server).editDocument @12"}},
		{"ownership check weakened", "internal/web/documents.go", handler,
			strings.Replace(handler, "doc.OwnerID != user.ID && !user.IsAdmin", "doc.OwnerID != user.ID && false", 1),
			[]string{"auth_logic_changed (*Server).editDocument @12"}},
		{"branch that returns 403 removed", "internal/web/documents.go", handler,
			strings.Replace(handler, "\tif !s.limiter.Allow() {\n\t\thttp.Error(w, \"denied\", http.StatusForbidden)\n\t\treturn\n\t}\n", "", 1),
			[]string{"auth_logic_changed (*Server).editDocument @23"}},
		{"method check removed is not access", "internal/web/documents.go", handler,
			strings.Replace(handler, "\tif r.Method != http.MethodPost {\n\t\thttp.Error(w, \"method\", http.StatusMethodNotAllowed)\n\t\treturn\n\t}\n", "", 1),
			nil},
		{"validation on user fields is not access", "internal/web/documents.go", handler,
			strings.Replace(handler, `doc.Title == "" || user.Name == ""`, `doc.Title == ""`, 1),
			nil},
		{"client checking a response for 401 is not access", "cmd/check.go",
			"package main\n\nfunc check(resp *http.Response) error {\n\tif resp.StatusCode == http.StatusUnauthorized {\n\t\treturn errors.New(\"unauthorized\")\n\t}\n\treturn nil\n}\n",
			"package main\n\nfunc check(resp *http.Response) error {\n\treturn nil\n}\n", nil},
		{"ID equality in a lookup is not access", "server/channels/store/storetest/reaction_store.go",
			"package storetest\n\nfunc saved(rs []*Reaction, r *Reaction) bool {\n\tfor _, got := range rs {\n\t\tsame := got.UserId == r.UserId && got.PostId == r.PostId && got.EmojiName == r.EmojiName\n\t\tuse(same)\n\t}\n\treturn false\n}\n",
			"package storetest\n\nfunc saved(rs []*Reaction, r *Reaction) bool {\n\tfor _, got := range rs {\n\t\tsame := got.UserId == r.UserId && got.EmojiName == r.EmojiName\n\t\tuse(same)\n\t}\n\treturn false\n}\n", nil},
		{"ID equality in a lookup loop is not access", "store/members.go",
			"package store\n\nfunc find(ms []M, userID string) *M {\n\tfor i, m := range ms {\n\t\tif m.UserID == userID {\n\t\t\treturn &ms[i]\n\t\t}\n\t}\n\treturn nil\n}\n",
			"package store\n\nfunc find(ms []M, userID string) *M {\n\tfor i := range ms {\n\t\tif ms[i].UserID == userID {\n\t\t\treturn &ms[i]\n\t\t}\n\t}\n\treturn nil\n}\n", nil},
		{"re-indented access check is kept", "internal/web/documents.go", handler,
			strings.Replace(handler, "\tif doc.OwnerID != user.ID && !user.IsAdmin {\n\t\thttp.Error(w, \"forbidden\", http.StatusForbidden)\n\t\treturn\n\t}\n",
				"\tif s.enforce {\n\t\tif doc.OwnerID != user.ID && !user.IsAdmin {\n\t\t\thttp.Error(w, \"forbidden\", http.StatusForbidden)\n\t\t\treturn\n\t\t}\n\t}\n", 1),
			nil},
		{"validation message saying forbidden is not access", "internal/web/names.go",
			"package web\n\nfunc check(n string) error {\n\tif strings.ContainsAny(n, \"/\") {\n\t\treturn errors.New(\"name has forbidden characters\")\n\t}\n\treturn nil\n}\n",
			"package web\n\nfunc check(n string) error {\n\tif strings.ContainsAny(n, \"/\\\\\") {\n\t\treturn errors.New(\"name has forbidden characters\")\n\t}\n\treturn nil\n}\n", nil},
		{"adding an access check is not reported", "internal/web/documents.go",
			strings.Replace(handler, "\tif doc.OwnerID != user.ID && !user.IsAdmin {\n\t\thttp.Error(w, \"forbidden\", http.StatusForbidden)\n\t\treturn\n\t}\n", "", 1),
			handler, nil},
	})

	const errs = `package store

import "log"

func Save(db DB) error {
	if err := db.Exec(); err != nil {
		return err
	}
	return nil
}
`
	run(t, []tc{
		{"error only logged", "store/store.go", errs,
			strings.Replace(errs, "\t\treturn err\n", "\t\tlog.Printf(\"save: %v\", err)\n", 1),
			[]string{"error_swallowed Save @6"}},
		{"error emptied", "store/store.go", errs,
			strings.Replace(errs, "\t\treturn err\n", "", 1),
			[]string{"error_swallowed Save @6"}},
		{"error returns nil", "store/store.go", errs,
			strings.Replace(errs, "\t\treturn err\n", "\t\treturn nil\n", 1),
			[]string{"error_swallowed Save @6"}},
		{"log.Fatal ends the program, so it does not swallow", "store/store.go", errs,
			strings.Replace(errs, "\t\treturn err\n", "\t\tlog.Fatal(err)\n", 1),
			nil},
		{"error wrapped is fine", "store/store.go", errs,
			strings.Replace(errs, "\t\treturn err\n", "\t\treturn fmt.Errorf(\"save: %w\", err)\n", 1),
			nil},
		{"blank-assigned call", "store/store.go", errs,
			strings.Replace(errs, "\treturn nil\n}", "\t_ = db.Close()\n\treturn nil\n}", 1),
			[]string{"error_swallowed Save @9"}},
		{"unchanged swallow elsewhere is not reported", "store/store.go",
			errs + "\nfunc Old(db DB) { _ = db.Close() }\n",
			strings.Replace(errs, "return nil", "return db.Flush()", 1) + "\nfunc Old(db DB) { _ = db.Close() }\n",
			nil},
	})

	const conc = `package work

func Run(jobs []Job) {
	for _, j := range jobs {
		j.Do()
	}
}
`
	run(t, []tc{
		{"goroutine added", "work/work.go", conc,
			strings.Replace(conc, "\t\tj.Do()", "\t\tgo j.Do()", 1),
			[]string{"concurrency_changed Run @5"}},
		{"mutex added", "work/work.go", conc,
			strings.Replace(conc, "\t\tj.Do()", "\t\tmu.Lock()\n\t\tj.Do()\n\t\tmu.Unlock()", 1),
			[]string{"concurrency_changed Run @5"}},
	})

	const sql = `package repo

const help = "select a file from the list"

func Find(db DB, id int) {
	db.Query("SELECT id, name FROM users WHERE id = $1", id)
}
`
	run(t, []tc{
		{"query changed", "repo/repo.go", sql,
			strings.Replace(sql, "WHERE id = $1", "WHERE id = $1 OR 1=1", 1),
			[]string{"sql_changed Find @6"}},
		{"edit on the line below a query is not a query change", "repo/repo.go",
			sql + "\nfunc Other() int { return 1 }\n",
			strings.Replace(sql+"\nfunc Other() int { return 1 }\n", "\tdb.Query(\"SELECT id, name FROM users WHERE id = $1\", id)\n", "\tdb.Query(\"SELECT id, name FROM users WHERE id = $1\", id)\n\tlog(id)\n", 1),
			nil},
		{"replacing the line below a query is not a query change", "repo/repo.go",
			strings.Replace(sql, "id)\n}", "id)\n\ttrace(1)\n}", 1),
			strings.Replace(sql, "id)\n}", "id)\n\ttrace(2)\n}", 1),
			nil},
		{"prose is not SQL", "repo/repo.go", sql,
			strings.Replace(sql, "select a file from the list", "select a file from the menu", 1),
			nil},
	})

	const test = `package user

import "testing"

func TestCanAccess(t *testing.T) {
	if !CanAccess(owner, doc) {
		t.Fatal("owner denied")
	}
	if CanAccess(stranger, doc) {
		t.Error("stranger allowed")
	}
	t.Run("admin", func(t *testing.T) {
		require.True(t, CanAccess(admin, doc))
	})
}

func TestOther(t *testing.T) {
	assert.Equal(t, 1, Other())
}
`
	run(t, []tc{
		{"assertion removed", "user/user_test.go", test,
			strings.Replace(test, "\tif CanAccess(stranger, doc) {\n\t\tt.Error(\"stranger allowed\")\n\t}\n", "", 1),
			[]string{"assertion_removed TestCanAccess @10"}},
		{"test skipped", "user/user_test.go", test,
			strings.Replace(test, "func TestOther(t *testing.T) {\n", "func TestOther(t *testing.T) {\n\tt.Skip(\"flaky\")\n", 1),
			[]string{"test_skipped TestOther @18"}},
		{"test deleted", "user/user_test.go", test,
			strings.Replace(test, "\nfunc TestOther(t *testing.T) {\n\tassert.Equal(t, 1, Other())\n}\n", "", 1),
			[]string{"test_deleted TestOther @17"}},
		{"subtest deleted", "user/user_test.go", test,
			strings.Replace(test, "\tt.Run(\"admin\", func(t *testing.T) {\n\t\trequire.True(t, CanAccess(admin, doc))\n\t})\n", "", 1),
			[]string{"assertion_removed TestCanAccess @13", "test_deleted TestCanAccess/admin @12"}},
		{"new assertion is fine", "user/user_test.go", test,
			strings.Replace(test, "\tassert.Equal(t, 1, Other())\n", "\tassert.Equal(t, 1, Other())\n\tassert.Equal(t, 2, Other2())\n", 1),
			nil},
	})

	run(t, []tc{
		{"syntax error on changed line", "x/x.go", "package x\n\nfunc F() {}\n", "package x\n\nfunc F() {\n",
			[]string{"parse_error  @3"}},
		{"Go 1.26 new(expr) parses", "x/x.go", "package x\n\nfunc F(p P) {\n}\n",
			"package x\n\nfunc F(p P) {\n\ta := new(int(p.GetLimit()))\n\tb := new(50)\n\tuse(a, b, new(T))\n}\n", nil},
		{"generated files are skipped", "x/x.pb.go", "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage x\n\nfunc F(a int) {}\n",
			"// Code generated by protoc-gen-go. DO NOT EDIT.\npackage x\n\nfunc F(a string) {}\n", nil},
	})
}

func TestTypeScript(t *testing.T) {
	const api = `import { db } from "./db";

export function getUser(id: number): Promise<User> {
  return db.find(id);
}

export class Store {
  private cache = new Map();
  get(key: string): string { return this.cache.get(key); }
  private load(): void {}
}

export const toSlug = (s: string): string => s.toLowerCase();
export { helper as publicHelper } from "./helper";
export interface User { id: number; name: string }
`
	run(t, []tc{
		{"param type changes signature", "src/api.ts", api,
			strings.Replace(api, "getUser(id: number)", "getUser(id: string)", 1),
			[]string{"exported_signature_changed getUser @3"}},
		{"body edit does not", "src/api.ts", api,
			strings.Replace(api, "return db.find(id);", "return db.findOne(id);", 1),
			nil},
		{"private member does not", "src/api.ts", api,
			strings.Replace(api, "private load(): void {}", "private load(force: boolean): void {}", 1),
			nil},
		{"public method does", "src/api.ts", api,
			strings.Replace(api, "get(key: string): string", "get(key: string, d?: string): string", 1),
			[]string{"exported_signature_changed Store @7"}},
		{"arrow export signature", "src/api.ts", api,
			strings.Replace(api, "(s: string): string => s.toLowerCase()", "(s: string, sep = \"-\"): string => s.toLowerCase()", 1),
			[]string{"exported_signature_changed toSlug @13"}},
		{"interface field", "src/api.ts", api,
			strings.Replace(api, "name: string }", "name?: string }", 1),
			[]string{"exported_signature_changed User @15"}},
		{"re-export removed", "src/api.ts", api,
			strings.Replace(api, "export { helper as publicHelper } from \"./helper\";\n", "", 1),
			[]string{"exported_symbol_removed publicHelper @14"}},
	})

	const auth = `export const canEdit = (user: User, doc: Doc) => user.id === doc.ownerId;

export function requireAdmin(user: User) {
  if (!user.isAdmin) {
    throw new Forbidden();
  }
}

export function formatAccessibleLabel(s: string) {
  if (!s) return "";
  return s.trim();
}
`
	run(t, []tc{
		{"arrow auth predicate", "src/perm.ts", auth,
			strings.Replace(auth, "user.id === doc.ownerId", "user.id === doc.ownerId || user.isStaff", 1),
			[]string{"auth_logic_changed canEdit @1"}},
		{"auth if changed", "src/perm.ts", auth,
			strings.Replace(auth, "if (!user.isAdmin) {", "if (!user.isAdmin && !user.isOwner) {", 1),
			[]string{"auth_logic_changed requireAdmin @4"}},
		{"accessible is not access", "src/perm.ts", auth,
			strings.Replace(auth, "return s.trim();", "return s.trim().toLowerCase();", 1),
			nil},
	})

	const actions = `export function DocumentActions({ user, doc, items, onDelete }: Props) {
  return (
    <div className="flex gap-2">
      <ShareButton doc={doc} />
      {user.isAdmin && <DeleteButton onClick={() => onDelete(doc.id)} />}
      {items.length > 0 && <List items={items} />}
      {doc.authorId === user.id ? <EditButton /> : null}
    </div>
  );
}
`
	run(t, []tc{
		// Thread A's testdata/injection/admin-button.txt.
		{"admin guard removed from JSX", "frontend/src/components/DocumentActions.tsx", actions,
			strings.Replace(actions, "{user.isAdmin && <DeleteButton", "{<DeleteButton", 1),
			[]string{"auth_logic_changed DocumentActions @5"}},
		{"ownership ternary changed", "frontend/src/components/DocumentActions.tsx", actions,
			strings.Replace(actions, "doc.authorId === user.id ?", "true ?", 1),
			[]string{"auth_logic_changed DocumentActions @7"}},
		{"editing the guarded element keeps the guard", "frontend/src/components/DocumentActions.tsx", actions,
			strings.Replace(actions, "<DeleteButton onClick", "<DeleteButton label=\"Remove\" onClick", 1),
			nil},
		{"ownership used for styling is not access", "src/Bubble.tsx",
			"export const Bubble = ({ msg, me }: P) => <div className={msg.authorId === me.id ? 'right' : 'left'}>{msg.text}</div>;\n",
			"export const Bubble = ({ msg, me }: P) => <div className={msg.authorId === me.id ? 'end' : 'start'}>{msg.text}</div>;\n",
			nil},
		{"client handling a 401 is not access", "src/api.ts",
			"export async function get(res: Res) {\n  if (res.statusCode === 401) {\n    throw new UnauthorizedError('login again');\n  }\n}\n",
			"export async function get(res: Res) {\n}\n", nil},
		{"stream canRead is not access", "src/stream.ts",
			"export function pump(s: S) {\n  if (!s.canRead || s.closed) {\n    return;\n  }\n  s.read();\n}\n",
			"export function pump(s: S) {\n  if (s.closed) {\n    return;\n  }\n  s.read();\n}\n", nil},
		{"non-access guard removed", "frontend/src/components/DocumentActions.tsx", actions,
			strings.Replace(actions, "{items.length > 0 && <List items={items} />}", "<List items={items} />", 1),
			nil},
	})

	const errs = `export async function load(id: string) {
  try {
    return await fetchThing(id);
  } catch (err) {
    throw new LoadError(id, err);
  }
}
`
	run(t, []tc{
		{"empty catch", "src/load.ts", errs,
			strings.Replace(errs, "    throw new LoadError(id, err);\n", "", 1),
			[]string{"error_swallowed load @4"}},
		{"catch only logs", "src/load.ts", errs,
			strings.Replace(errs, "throw new LoadError(id, err);", "console.error(err);", 1),
			[]string{"error_swallowed load @4"}},
		{"promise catch ignored", "src/load.ts", errs,
			errs + "export const warm = () => fetchThing(\"a\").catch(() => {});\n",
			[]string{"error_swallowed warm @8"}},
	})

	const comp = `import { useEffect, useState } from "react";

export default function Profile({ userId }: Props) {
  const [user, setUser] = useState<User | null>(null);
  useEffect(() => {
    load(userId).then(setUser);
  }, [userId]);
  return <div className="p-4">{user && <Card user={user} />}</div>;
}
`
	run(t, []tc{
		{"effect deps changed", "src/Profile.tsx", comp,
			strings.Replace(comp, "}, [userId]);", "}, []);", 1),
			[]string{"concurrency_changed Profile @5"}},
		{"markup change has no findings", "src/Profile.tsx", comp,
			strings.Replace(comp, `className="p-4"`, `className="p-6"`, 1),
			nil},
		{"Promise.all added", "src/Profile.tsx", comp,
			strings.Replace(comp, "load(userId).then(setUser);", "Promise.all([load(userId), prefs(userId)]).then(([u]) => setUser(u));", 1),
			[]string{"concurrency_changed Profile @6"}},
	})

	const sql = "export const byEmail = (email: string) =>\n  sql`select * from users where email = ${email}`;\n"
	run(t, []tc{
		{"tagged SQL changed", "src/q.ts", sql, strings.Replace(sql, "where email", "where lower(email)", 1),
			[]string{"sql_changed byEmail @2"}},
	})

	const spec = `describe("canEdit", () => {
  it("allows the owner", () => {
    expect(canEdit(owner, doc)).toBe(true);
  });
  it("denies others", () => {
    expect(canEdit(other, doc)).toBe(false);
    expect(canEdit(guest, doc)).toBe(false);
  });
});
`
	run(t, []tc{
		{"assertion removed", "src/perm.test.ts", spec,
			strings.Replace(spec, "    expect(canEdit(guest, doc)).toBe(false);\n", "", 1),
			[]string{"assertion_removed canEdit > denies others @7"}},
		{"test skipped", "src/perm.test.ts", spec,
			strings.Replace(spec, `it("denies others"`, `it.skip("denies others"`, 1),
			[]string{"test_skipped canEdit > denies others @5"}},
		{"test focused", "src/perm.test.ts", spec,
			strings.Replace(spec, `it("allows the owner"`, `it.only("allows the owner"`, 1),
			[]string{"test_skipped canEdit > allows the owner @2"}},
		{"test deleted", "src/perm.test.ts", spec,
			strings.Replace(spec, "  it(\"allows the owner\", () => {\n    expect(canEdit(owner, doc)).toBe(true);\n  });\n", "", 1),
			[]string{"test_deleted canEdit > allows the owner @2"}},
	})

	const each = "test.each([[1, 2]])(\"adds %i\", (a, b) => {\n  expect(add(a, b)).toBe(a + b);\n});\n"
	run(t, []tc{
		{"adding a test.each row deletes nothing", "src/add.test.ts", each,
			strings.Replace(each, "[[1, 2]]", "[[1, 2], [3, 4]]", 1), nil},
		{"it.todo is not a skip", "src/add.test.ts", each, each + "it.todo(\"subtracts\");\n", nil},
	})

	run(t, []tc{
		{"syntax error on changed line", "src/x.ts", "export const a = 1;\n", "export const a = (1;\n",
			[]string{"parse_error  @1"}},
	})
}

func TestGeneratedMarkerIsNoted(t *testing.T) {
	src := "// Code generated by hand. DO NOT EDIT.\npackage auth\n\nfunc init() {\n\tbypass = func() bool { return true }\n}\n"
	fs := analyze(t, change("internal/auth/hooks_gen.go", "", src))
	if len(fs) != 1 || !strings.Contains(fs[0].Detail, "says it is generated") {
		t.Fatalf("want one finding noting the generated marker, got %+v", fs)
	}
}
