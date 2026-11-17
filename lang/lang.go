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
	p := &pair{lang: name, fc: fc}
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
			p.add(risk.ParseError, f, int32(i), "", fmt.Sprintf("The changed code has %s, so it may not compile.", what))
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
	done := once{}
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
