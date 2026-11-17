// Package lang reports structural evidence for changed Go and
// TypeScript/TSX/JavaScript files, such as changed exported signatures,
// edited conditions in auth functions, removed assertions, skipped tests,
// swallowed errors, concurrency changes, and SQL in strings.
//
// Parsing uses tree-sitter compiled to WebAssembly and run on wazero
// (lang/internal/tswasm), so the brisk binary needs no cgo.
package lang

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ronny/brisk/lang/internal/tswasm"
	"github.com/ronny/brisk/risk"
)

// maxFileBytes skips files whose parse would cost more than the assessment
// budget. Typical source files are far below it.
const maxFileBytes = 256 << 10

// maxPerKind caps findings of one kind in one file.
const maxPerKind = 5

// Backends returns the language backends brisk uses.
func Backends() []risk.LanguageBackend {
	return []risk.LanguageBackend{goBackend{}, tsBackend{}}
}

// file is one side (old or new) of a changed file.
type file struct {
	path    string
	src     []byte
	t       *tswasm.Tree
	end     []int32      // end[i] is one past the last preorder index in i's subtree
	changed map[int]bool // 0-based rows that changed on this side
}

func parse(l tswasm.Lang, path string, src []byte, changed map[int]bool) (*file, error) {
	t, err := tswasm.Parse(l, src)
	if err != nil {
		return nil, err
	}
	f := &file{path: path, src: src, t: t, changed: changed, end: make([]int32, len(t.Nodes))}
	for i := len(t.Nodes) - 1; i >= 0; i-- {
		if f.end[i] == 0 {
			f.end[i] = int32(i + 1)
		}
		if p := t.Nodes[i].Parent; p >= 0 && f.end[i] > f.end[p] {
			f.end[p] = f.end[i]
		}
	}
	return f, nil
}

func (f *file) kind(i int32) string { return f.t.Kind(i) }
func (f *file) text(i int32) string {
	if i < 0 {
		return ""
	}
	return f.t.Text(i)
}
func (f *file) field(i int32, name string) int32 {
	if i < 0 {
		return -1
	}
	return f.t.ChildByField(i, name)
}
func (f *file) parent(i int32) int32 { return f.t.Nodes[i].Parent }

// named returns i's named children, without comments.
func (f *file) named(i int32) []int32 {
	var out []int32
	for _, c := range f.t.Children[i] {
		if f.t.Nodes[c].Named() && f.kind(c) != "comment" {
			out = append(out, c)
		}
	}
	return out
}

// touches reports whether any row of node i changed.
func (f *file) touches(i int32) bool {
	n := &f.t.Nodes[i]
	return f.rowsChanged(int(n.StartRow), int(n.EndRow))
}

// startChanged reports whether the first row of node i changed.
func (f *file) startChanged(i int32) bool { return f.changed[int(f.t.Nodes[i].StartRow)] }

func (f *file) rowsChanged(from, to int) bool {
	if to-from > len(f.changed) {
		for r := range f.changed {
			if r >= from && r <= to {
				return true
			}
		}
		return false
	}
	for r := from; r <= to; r++ {
		if f.changed[r] {
			return true
		}
	}
	return false
}

// tokens joins the leaf tokens of node i that start before limit (a byte
// offset), skipping comments, so formatting changes do not count.
func (f *file) tokens(i int32, limit uint32) string {
	var b strings.Builder
	for k := i; k < f.end[i]; k++ {
		n := &f.t.Nodes[k]
		if n.StartByte >= limit {
			break
		}
		if f.kind(k) == "comment" {
			k = f.end[k] - 1
			continue
		}
		if len(f.t.Children[k]) == 0 && n.EndByte > n.StartByte {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.Write(f.src[n.StartByte:n.EndByte])
		}
	}
	return b.String()
}

func (f *file) all(i int32) string { return f.tokens(i, ^uint32(0)) }

// upTo returns the tokens of node i before its field (such as "body"), or
// all of them when the field is absent.
func (f *file) upTo(i int32, field string) string {
	if b := f.field(i, field); b >= 0 {
		return f.tokens(i, f.t.Nodes[b].StartByte)
	}
	return f.all(i)
}

func line(f *file, i int32) (int, int) {
	n := &f.t.Nodes[i]
	return int(n.StartRow) + 1, int(n.EndRow) + 1
}

// changedRows returns the rows (0-based) that changed on each side.
func changedRows(fc risk.FileChange) (old, new map[int]bool) {
	old, new = map[int]bool{}, map[int]bool{}
	for _, h := range fc.Hunks {
		if len(h.Lines) == 0 {
			for r := h.OldStart; r < h.OldStart+h.OldLines; r++ {
				old[r-1] = true
			}
			for r := h.NewStart; r < h.NewStart+h.NewLines; r++ {
				new[r-1] = true
			}
			continue
		}
		o, n := h.OldStart, h.NewStart
		// A pure deletion (a run of "-" lines not followed by "+" lines) also
		// changes the new line just above it, such as an if whose body lost
		// its return. A replacement already marks its own new lines.
		if h.NewLines == 0 {
			// git -U0: a pure deletion's NewStart is the line before it.
			if h.NewStart > 0 {
				new[h.NewStart-1] = true
			}
			n = 0 // nothing else to mark on the new side
		}
		deleting := false
		endDeletion := func() {
			if deleting && n > 1 {
				new[n-2] = true
			}
			deleting = false
		}
		for _, l := range h.Lines {
			if l == "" {
				l = " "
			}
			switch l[0] {
			case '-':
				old[o-1] = true
				deleting = true
				o++
			case '+':
				deleting = false
				new[n-1] = true
				n++
			case '\\':
			default:
				endDeletion()
				o++
				n++
			}
		}
		endDeletion()
	}
	return old, new
}

var generated = regexp.MustCompile(`(?m)^(//|/\*|\s\*).*(Code generated .* DO NOT EDIT|@generated)`)

func isGenerated(src []byte) bool {
	if len(src) > 2048 {
		src = src[:2048]
	}
	return generated.Match(src)
}

// pair is the parsed old and new file. Either may be nil for added or
// deleted files.
type pair struct {
	old, new *file
	lang     string
	fc       risk.FileChange
	out      []risk.Finding
	authed   once // functions already given an auth_logic_changed finding
}

func load(ctx context.Context, fc risk.FileChange, l tswasm.Lang, name string) (*pair, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fc.Binary || len(fc.Old) > maxFileBytes || len(fc.New) > maxFileBytes ||
		isGenerated(fc.New) || (fc.Status == risk.Deleted && isGenerated(fc.Old)) {
		return nil, nil
	}
	oldRows, newRows := changedRows(fc)
	p := &pair{lang: name, fc: fc, authed: once{}}
	var err error
	if fc.Status != risk.Added && len(fc.Old) > 0 {
		path := fc.OldPath
		if path == "" {
			path = fc.Path
		}
		if p.old, err = parse(l, path, fc.Old, oldRows); err != nil {
			return nil, err
		}
	}
	if fc.Status != risk.Deleted && len(fc.New) > 0 {
		if p.new, err = parse(l, fc.Path, fc.New, newRows); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *pair) add(kind risk.FindingKind, f *file, i int32, symbol, detail string) {
	start, end := line(f, i)
	p.addLines(kind, start, end, symbol, detail)
}

func (p *pair) addLines(kind risk.FindingKind, start, end int, symbol, detail string) {
	p.out = append(p.out, risk.Finding{
		Kind: kind, Path: p.fc.Path, StartLine: start, EndLine: end,
		Symbol: symbol, Detail: detail, Language: p.lang,
	})
}

// parseErrors reports a syntax error on changed lines of the new file.
func (p *pair) parseErrors() {
	f := p.new
	if f == nil || !f.t.Nodes[0].HasError() {
		return
	}
	for i := range f.t.Nodes {
		n := &f.t.Nodes[i]
		if (n.IsError() || n.Missing()) && f.touches(int32(i)) {
			what := "a syntax error"
			if n.Missing() {
				what = fmt.Sprintf("a missing %q", f.kind(int32(i)))
			}
			p.add(risk.ParseError, f, int32(i), "", fmt.Sprintf("brisk could not parse the changed code (%s): it may not compile, or it uses syntax the parser does not know.", what))
			return
		}
	}
}

// compareAPI compares exported declarations between old and new.
func (p *pair) compareAPI(old, new map[string]decl) {
	for _, k := range sortedKeys(old) {
		o := old[k]
		n, ok := new[k]
		switch {
		case !ok:
			p.addLines(risk.ExportedSymbolRemoved, o.sigStart, o.sigEnd, o.name, fmt.Sprintf("Removes or renames exported %s %s.", o.what, o.name))
		case o.sig != n.sig:
			p.addLines(risk.ExportedSignatureChanged, n.sigStart, n.sigEnd, n.name, fmt.Sprintf("Changes the signature of exported %s %s.", n.what, n.name))
		}
	}
}

// decl is an exported declaration.
type decl struct {
	name             string // for people, e.g. "(*Server).Handle"
	what             string // "function", "method", "type", ...
	sig              string // normalised tokens; a change here is a signature change
	node             int32
	sigStart, sigEnd int // 1-based lines of the signature
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// finish caps findings per kind and orders them by line.
func (p *pair) finish() []risk.Finding {
	sort.SliceStable(p.out, func(a, b int) bool {
		if p.out[a].Kind != p.out[b].Kind {
			return p.out[a].Kind < p.out[b].Kind
		}
		return p.out[a].StartLine < p.out[b].StartLine
	})
	var out []risk.Finding
	count := map[risk.FindingKind]int{}
	for _, f := range p.out {
		count[f.Kind]++
		if count[f.Kind] <= maxPerKind {
			out = append(out, f)
		}
	}
	for i := range out {
		if c := count[out[i].Kind]; c > maxPerKind && (i+1 == len(out) || out[i+1].Kind != out[i].Kind) {
			out[i].Detail += fmt.Sprintf(" (%d more like this in the file)", c-maxPerKind)
		}
	}
	return out
}

// authName matches function names that suggest authentication or
// authorisation. Short words (acl, role, token, ...) must be whole words in
// the name, so "oracle", "controller", "canonical", and "accessible" do not match.
var authName = regexp.MustCompile(`(?i:auth|permission|privilege|rbac|polic(y|ies)|login|logout|sign_?in|session|jwt|oauth|saml|sso|passw(or)?d|credential|csrf|admin|guard|forbid)` +
	`|(^|[.)_])(?i:acl|access|roles?|tokens?|verify|allow(ed)?|deny|denied)([A-Z_.(]|$)` +
	`|[a-z0-9](Acl|ACL|Access|Roles?|Tokens?|Verify|Allow(ed)?|Deny|Denied)([A-Z_.(]|$)` +
	`|(^|[.)_])(can|may|Can|May)[A-Z_]`)
var authPath = regexp.MustCompile(`(?i)(^|/)(auth|authz|authn|permissions?|acl|rbac|polic(y|ies)|session|login|security|guards?)(/|\.|_|-)`)

// authChanges reports, once per function and preferring the new file, a changed check, condition, or
// return (hit) inside a function whose name or file suggests auth code.
func (p *pair) authChanges(hit func(*file, int32) bool, symbol func(*file, int32) string) {
	done := p.authed
	for _, f := range []*file{p.new, p.old} {
		if f == nil {
			continue
		}
		pathAuth := authPath.MatchString(f.path)
		for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
			if !hit(f, i) {
				continue
			}
			sym := symbol(f, i)
			if sym == "" || !(pathAuth || authName.MatchString(sym)) || !done.first(sym) {
				continue
			}
			detail := fmt.Sprintf("Changes a check, condition, or return in %s, which looks like auth code.", sym)
			if f == p.old {
				detail = fmt.Sprintf("Removes or rewrites a check, condition, or return in %s, which looks like auth code.", sym)
			}
			p.add(risk.AuthLogicChanged, f, i, sym, detail)
		}
	}
}

var (
	// accessFlag matches admin, role, and permission checks such as
	// user.IsAdmin, hasPermission(...), canEdit, or role === "admin".
	accessFlag = regexp.MustCompile(`(?i)\b(is_?(admin|superuser|super_?admin|staff|owner|moderator)|has_?(roles?|permissions?|perms?|access|scopes?)|can_?(edit|delete|view|manage|access|admin))\b` +
		`|(?i)\broles?\b[^&|]{0,40}\b(admin|owner|superuser)\b`)
	// idLike is an identifier that holds an ID, such as doc.OwnerID or userId.
	idLike = regexp.MustCompile(`(^|[._])(id|ID|Id)$|[a-z0-9](ID|Id)$|_id$`)
	// whoseID names the subject of an ID comparison.
	whoseID = regexp.MustCompile(`(?i)(owner|user|author|creator|account|tenant|org|member|customer)`)
	// denies matches code that refuses access: status constants and error
	// identifiers, not free text such as "forbidden characters".
	denies = regexp.MustCompile(`StatusForbidden|StatusUnauthorized|\b(401|403)\b|codes\.(PermissionDenied|Unauthenticated)|ErrPermission|PermissionDenied|Forbidden(Error|Exception)?\b|Unauthori[sz]ed(Error|Exception)\b|ErrUnauthori[sz]ed|ErrForbidden`)
	// responseStatus matches a condition that reads a response or error
	// status: a client handling 401, not a server refusing.
	responseStatus = regexp.MustCompile(`(?i)status_?code|\.status\b|\bstatus\s*[!=]==?|\.code\s*[!=]==?`)
	// emptyBranch is a ternary branch that renders nothing.
	emptyBranch = regexp.MustCompile(`^(null|undefined|false|nil|""|''|<></>)$`)
)

// accessCondition reports whether the expression at i tests an admin, role,
// or permission flag, or (with ids) compares owner or user IDs.
func accessCondition(f *file, i int32, ids bool) bool {
	if i < 0 {
		return false
	}
	if accessFlag.MatchString(f.text(i)) {
		return true
	}
	if !ids {
		return false
	}
	for k := i; k < f.end[i]; k++ {
		if f.kind(k) != "binary_expression" {
			continue
		}
		switch f.text(f.field(k, "operator")) {
		case "==", "!=", "===", "!==":
		default:
			continue
		}
		l, r := f.text(f.field(k, "left")), f.text(f.field(k, "right"))
		if idLike.MatchString(l) && idLike.MatchString(r) && (whoseID.MatchString(l) || whoseID.MatchString(r)) {
			return true
		}
	}
	return false
}

// access describes the access check at node i, wherever it is, and returns
// the node holding its condition, or ("", -1). It recognises:
//   - if statements that test an admin, role, or permission flag, or that
//     compare owner or user IDs and refuse access in a short branch;
//   - short if branches that refuse access with 401, 403, or Forbidden,
//     unless the condition reads a response status;
//   - && guards on a flag, such as JSX {user.isAdmin && <Delete />};
//   - ternaries on a flag, or on an ID comparison with an empty branch,
//     such as {doc.authorId === user.id ? <Edit /> : null}.
//
// ID equality alone is not enough: it is as often a lookup or a style
// choice as an ownership check.
func access(f *file, i int32) (string, int32) {
	switch f.kind(i) {
	case "if_statement":
		cond, body := f.field(i, "condition"), f.field(i, "consequence")
		if cond < 0 {
			return "", -1
		}
		refuses := body >= 0 && f.t.Nodes[body].EndRow-f.t.Nodes[body].StartRow <= 6 && denies.MatchString(f.text(body))
		if accessCondition(f, cond, refuses) {
			return fmt.Sprintf("an access check (%s)", short(f.text(cond))), cond
		}
		if refuses && !responseStatus.MatchString(f.text(cond)) {
			return fmt.Sprintf("a check that refuses access (%s)", short(f.text(cond))), cond
		}
	case "binary_expression":
		left := f.field(i, "left")
		if left < 0 || f.text(f.field(i, "operator")) != "&&" {
			return "", -1
		}
		if p := f.parent(i); p >= 0 && f.kind(p) == "binary_expression" && f.text(f.field(p, "operator")) == "&&" {
			return "", -1 // report the outermost && once
		}
		if accessCondition(f, left, false) {
			return fmt.Sprintf("an access guard (%s)", short(f.text(left))), left
		}
	case "ternary_expression":
		cond := f.field(i, "condition")
		gates := emptyBranch.MatchString(f.text(f.field(i, "consequence"))) || emptyBranch.MatchString(f.text(f.field(i, "alternative")))
		if accessCondition(f, cond, gates) {
			return fmt.Sprintf("an access check (%s)", short(f.text(cond))), cond
		}
	}
	return "", -1
}

// accessChanges runs before authChanges, so its more specific finding wins
// for a function both would report. It reports, once per function, access checks in the old file
// that the change removed or rewrote. A check whose condition survives
// unchanged in the new file (re-indented, moved, or with other edits on its
// line) is not reported, and checks that only exist in the new file are new
// code, not weakened code.
func (p *pair) accessChanges(symbol func(*file, int32) string) {
	if p.old == nil {
		return
	}
	kept := map[string]bool{}
	if f := p.new; f != nil {
		for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
			if what, cond := access(f, i); what != "" {
				kept[f.all(cond)] = true
			}
		}
	}
	f, done := p.old, p.authed
	for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
		what, cond := access(f, i)
		if what == "" || kept[f.all(cond)] || !(f.startChanged(i) || f.touches(cond)) {
			continue
		}
		if sym := symbol(f, i); done.first(sym) {
			p.add(risk.AuthLogicChanged, f, i, sym, fmt.Sprintf("Removes or changes %s in %s.", what, orFile(sym)))
		}
	}
}

// concurrency reports, once per function, changed concurrency code. what
// returns a description for a changed node, or "".
func (p *pair) concurrency(what func(*file, int32) string, symbol func(*file, int32) string) {
	done := once{}
	for _, f := range []*file{p.new, p.old} {
		if f == nil {
			continue
		}
		for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
			w := what(f, i)
			if w == "" {
				continue
			}
			sym := symbol(f, i)
			if !done.first(sym) {
				continue
			}
			verb := "Adds or changes"
			if f == p.old {
				verb = "Removes or changes"
			}
			p.add(risk.ConcurrencyChanged, f, i, sym, fmt.Sprintf("%s %s in %s.", verb, w, orFile(sym)))
		}
	}
}

// sqlLoose and sqlStrict find SQL in string literals. Lowercase keywords
// need a second SQL marker, so English prose such as "select a file from
// the list" does not match.
var (
	sqlStrict = regexp.MustCompile(`\b(SELECT\b[\s\S]+\bFROM|INSERT\s+INTO|UPDATE\s+\S+\s+SET|DELETE\s+FROM|CREATE\s+(TABLE|INDEX|VIEW|UNIQUE)|ALTER\s+TABLE|DROP\s+(TABLE|INDEX|VIEW|COLUMN)|TRUNCATE\s+\w|MERGE\s+INTO)\b`)
	sqlLoose  = regexp.MustCompile(`(?i)\b(select\b[\s\S]+\bfrom|insert\s+into|update\s+\S+\s+set|delete\s+from|create\s+table|alter\s+table|drop\s+table)\b`)
	sqlMarker = regexp.MustCompile(`(?i)(\bwhere\b|\bjoin\b|\bvalues\b|\breturning\b|\border\s+by\b|\bgroup\s+by\b|\blimit\s+\d|\$\d|\?|@\w+|:\w+|;\s*$)`)
)

func isSQL(s string) bool {
	return sqlStrict.MatchString(s) || (sqlLoose.MatchString(s) && sqlMarker.MatchString(s))
}

// sqlStrings reports changed string literals that hold SQL, once per
// enclosing symbol, preferring the new file.
func (p *pair) sqlStrings(kinds map[string]bool, symbol func(*file, int32) string, tagged func(*file, int32) bool) {
	seen := map[string]bool{}
	for _, f := range []*file{p.new, p.old} {
		if f == nil {
			continue
		}
		for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
			if !kinds[f.kind(i)] || !f.touches(i) {
				continue
			}
			if !isSQL(f.text(i)) && !(tagged != nil && tagged(f, i)) {
				i = f.end[i] - 1
				continue
			}
			sym := symbol(f, i)
			if seen[sym] {
				continue
			}
			seen[sym] = true
			verb := "Changes"
			if f == p.old {
				verb = "Removes or changes"
			}
			p.add(risk.SQLChanged, f, i, sym, fmt.Sprintf("%s an SQL query in %s.", verb, orFile(sym)))
			i = f.end[i] - 1
		}
	}
}

func orFile(sym string) string {
	if sym == "" {
		return "this file"
	}
	return sym
}

// once reports each key the first time only.
type once map[string]bool

func (o once) first(key string) bool {
	if o[key] {
		return false
	}
	o[key] = true
	return true
}
