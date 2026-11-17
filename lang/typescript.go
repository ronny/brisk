package lang

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/ronny/brisk/lang/internal/tswasm"
	"github.com/ronny/brisk/risk"
)

// tsBackend handles TypeScript, TSX, and JavaScript. JavaScript and JSX are
// parsed with the TSX grammar, which accepts them.
type tsBackend struct{}

func (tsBackend) Name() string { return "typescript" }

func tsLang(p string) (tswasm.Lang, string, bool) {
	switch path.Ext(p) {
	case ".ts", ".mts", ".cts":
		return tswasm.TypeScript, "typescript", true
	case ".tsx":
		return tswasm.TSX, "tsx", true
	case ".js", ".jsx", ".mjs", ".cjs":
		return tswasm.TSX, "javascript", true
	}
	return 0, "", false
}

func (tsBackend) Supports(p string) bool {
	_, _, ok := tsLang(p)
	return ok && !strings.HasSuffix(p, ".min.js")
}

var tsTestPath = regexp.MustCompile(`([._-](test|spec)\.[cm]?[jt]sx?$)|(^|/)__tests__/`)

func (tsBackend) Analyze(ctx context.Context, fc risk.FileChange) ([]risk.Finding, error) {
	l, name, ok := tsLang(fc.Path)
	if !ok {
		return nil, nil
	}
	p, err := load(ctx, fc, l, name)
	if p == nil || err != nil {
		return nil, err
	}
	test := tsTestPath.MatchString(fc.Path)
	if name != "javascript" { // Flow and other JS dialects do not parse cleanly
		p.parseErrors()
	}
	if !test {
		p.compareAPI(tsAPI(p.old), tsAPI(p.new))
	}
	if !test {
		p.authChanges(tsAuthHit, tsSymbol)
	}
	if p.new != nil {
		p.tsSwallowed(p.new, test)
	}
	p.concurrency(tsConcurrencyHit, tsSymbol)
	p.sqlStrings(map[string]bool{"string": true, "template_string": true}, tsSymbol, tsSQLTagged)
	if test {
		p.tsTests()
	}
	return p.finish(), nil
}

// tsSymbol names the innermost function, method, or function-valued
// variable that contains node i, such as "Session.refresh" or "canEdit".
func tsSymbol(f *file, i int32) string {
	for ; i >= 0; i = f.parent(i) {
		switch f.kind(i) {
		case "function_declaration", "generator_function_declaration":
			return f.text(f.field(i, "name"))
		case "method_definition", "public_field_definition":
			if f.kind(i) == "public_field_definition" && !tsFunctionValue(f, f.field(i, "value")) {
				continue
			}
			name := f.text(f.field(i, "name"))
			if c := tsClassName(f, i); c != "" {
				return c + "." + name
			}
			return name
		case "variable_declarator":
			if n := f.field(i, "name"); n >= 0 && f.kind(n) == "identifier" && tsFunctionValue(f, f.field(i, "value")) {
				return f.text(n)
			}
		}
	}
	return ""
}

// tsFunctionValue reports whether v is a function, or a call that wraps one
// such as React.memo(() => ...) or useCallback(() => ...).
func tsFunctionValue(f *file, v int32) bool {
	if v < 0 {
		return false
	}
	switch f.kind(v) {
	case "arrow_function", "function_expression", "function", "generator_function":
		return true
	case "call_expression":
		for _, a := range f.named(f.field(v, "arguments")) {
			if tsFunctionValue(f, a) {
				return true
			}
		}
	}
	return false
}

func tsClassName(f *file, i int32) string {
	for ; i >= 0; i = f.parent(i) {
		switch f.kind(i) {
		case "class_declaration", "abstract_class_declaration", "class":
			if n := f.field(i, "name"); n >= 0 {
				return f.text(n)
			}
			return ""
		}
	}
	return ""
}

func tsAPI(f *file) map[string]decl {
	out := map[string]decl{}
	if f == nil {
		return out
	}
	put := func(key string, d decl, i int32, field string) {
		d.node = i
		d.sigStart, d.sigEnd = sigRows(f, i, field)
		out[key] = d
	}
	for _, e := range f.named(0) {
		if f.kind(e) != "export_statement" {
			continue
		}
		isDefault := false
		for _, c := range f.t.Children[e] {
			if f.kind(c) == "default" {
				isDefault = true
			}
		}
		d := f.field(e, "declaration")
		if d < 0 {
			if v := f.field(e, "value"); v >= 0 && isDefault {
				sig := ""
				if tsFunctionValue(f, v) && f.field(v, "body") >= 0 {
					sig = f.upTo(v, "body")
				}
				put("default", decl{name: "default export", what: "value", sig: sig}, e, "")
				continue
			}
			src := ""
			if s := f.field(e, "source"); s >= 0 {
				src = " from " + f.text(s)
			}
			for k := e; k < f.end[e]; k++ {
				if f.kind(k) != "export_specifier" {
					continue
				}
				name := f.text(f.field(k, "name"))
				if a := f.field(k, "alias"); a >= 0 {
					name = f.text(a)
				}
				put(name, decl{name: name, what: "export", sig: f.all(k) + src}, k, "")
			}
			continue
		}
		key := func(name string) string {
			if isDefault {
				return "default"
			}
			return name
		}
		name := f.text(f.field(d, "name"))
		switch f.kind(d) {
		case "function_declaration", "generator_function_declaration", "function_signature":
			put(key(name), decl{name: name, what: "function", sig: f.upTo(d, "body")}, d, "body")
		case "class_declaration", "abstract_class_declaration":
			put(key(name), decl{name: name, what: "class", sig: tsClassSig(f, d)}, d, "body")
		case "interface_declaration":
			put(key("type "+name), decl{name: name, what: "interface", sig: f.all(d)}, d, "")
		case "type_alias_declaration":
			put(key("type "+name), decl{name: name, what: "type", sig: f.all(d)}, d, "")
		case "enum_declaration":
			put(key(name), decl{name: name, what: "enum", sig: f.all(d)}, d, "")
		case "lexical_declaration", "variable_declaration":
			for _, v := range f.named(d) {
				n := f.field(v, "name")
				if f.kind(v) != "variable_declarator" || n < 0 || f.kind(n) != "identifier" {
					continue
				}
				sig, what := "", "constant"
				if t := f.field(v, "type"); t >= 0 {
					sig = f.all(t)
				}
				if val := f.field(v, "value"); val >= 0 && f.field(val, "body") >= 0 && tsFunctionValue(f, val) {
					what = "function"
					sig += " = " + f.upTo(val, "body")
				}
				put(f.text(n), decl{name: f.text(n), what: what, sig: sig}, v, "value")
			}
		}
	}
	return out
}

// tsClassSig is the part of a class that callers see: its heritage and the
// signatures of members that are not private.
func tsClassSig(f *file, c int32) string {
	parts := []string{f.upTo(c, "body")}
	body := f.field(c, "body")
	if body < 0 {
		return parts[0]
	}
	for _, m := range f.named(body) {
		private := false
		for _, ch := range f.t.Children[m] {
			if k := f.kind(ch); (k == "accessibility_modifier" && f.text(ch) != "public") || k == "private_property_identifier" {
				private = true
			}
		}
		if private {
			continue
		}
		switch f.kind(m) {
		case "method_definition", "method_signature", "abstract_method_signature":
			parts = append(parts, f.upTo(m, "body"))
		case "public_field_definition":
			parts = append(parts, f.upTo(m, "value"))
		}
	}
	return strings.Join(parts, "; ")
}

func tsAuthHit(f *file, i int32) bool {
	switch f.kind(i) {
	case "if_statement":
		c := f.field(i, "condition")
		return f.startChanged(i) || (c >= 0 && f.touches(c))
	case "return_statement":
		return f.touches(i)
	case "switch_case", "switch_default":
		return f.startChanged(i)
	case "arrow_function":
		// const canEdit = (u) => u.role === "admin"
		b := f.field(i, "body")
		return b >= 0 && f.kind(b) != "statement_block" && f.touches(b)
	}
	return false
}

var tsLogCall = regexp.MustCompile(`^(console|logger|log|logging|this\.logger|this\.log|Sentry)\.\w+$|\.(log|warn|error|info|debug|trace|captureException)$`)

// tsSwallowed reports catch blocks that are empty or only log, and
// .catch(() => {}) handlers, on changed lines.
func (p *pair) tsSwallowed(f *file, test bool) {
	for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
		switch f.kind(i) {
		case "catch_clause":
			body := f.field(i, "body")
			if body < 0 || !(f.startChanged(i) || f.touches(body)) {
				continue
			}
			stmts := f.named(body)
			why := ""
			switch {
			case len(stmts) == 0:
				why = "an empty catch block"
			case !test && allCalls(f, stmts, tsLogCall):
				why = "a catch block that only logs"
			}
			if why != "" {
				sym := tsSymbol(f, i)
				p.add(risk.ErrorSwallowed, f, i, sym, fmt.Sprintf("Catches errors in %s with %s, so the error is lost.", orFile(sym), why))
			}
		case "call_expression":
			fn := f.field(i, "function")
			if fn < 0 || f.kind(fn) != "member_expression" || f.text(f.field(fn, "property")) != "catch" || !f.touches(i) {
				continue
			}
			args := f.named(f.field(i, "arguments"))
			if len(args) != 1 || !tsFunctionValue(f, args[0]) {
				continue
			}
			body := f.field(args[0], "body")
			if body < 0 {
				continue
			}
			empty := false
			switch f.kind(body) {
			case "statement_block":
				empty = len(f.named(body)) == 0
			case "null", "undefined":
				empty = true
			}
			if empty {
				sym := tsSymbol(f, i)
				p.add(risk.ErrorSwallowed, f, i, sym, fmt.Sprintf("Adds a .catch handler in %s that ignores the error.", orFile(sym)))
			}
		}
	}
}

var tsPromiseCombinator = map[string]bool{"all": true, "race": true, "allSettled": true, "any": true}
var tsEffectHook = regexp.MustCompile(`^(React\.)?(useEffect|useLayoutEffect|useInsertionEffect|useCallback|useMemo|useImperativeHandle)$`)

// tsConcurrencyHit describes a changed Promise combinator, effect hook
// dependency list, worker, or Atomics use at node i.
func tsConcurrencyHit(f *file, i int32) string {
	switch f.kind(i) {
	case "call_expression":
		fn := f.field(i, "function")
		if fn < 0 {
			return ""
		}
		callee := f.text(fn)
		if f.kind(fn) == "member_expression" && f.startChanged(i) {
			obj, prop := f.text(f.field(fn, "object")), f.text(f.field(fn, "property"))
			if obj == "Promise" && tsPromiseCombinator[prop] {
				return "Promise." + prop
			}
			if obj == "Atomics" {
				return "Atomics use"
			}
		}
		if tsEffectHook.MatchString(callee) {
			args := f.named(f.field(i, "arguments"))
			if len(args) >= 2 && f.kind(args[len(args)-1]) == "array" && f.touches(args[len(args)-1]) {
				return "the dependency list of " + callee
			}
		}
	case "new_expression":
		if c := f.field(i, "constructor"); c >= 0 && f.startChanged(i) {
			switch f.text(c) {
			case "Worker", "SharedWorker", "SharedArrayBuffer", "BroadcastChannel", "MessageChannel":
				return "a " + f.text(c)
			}
		}
	}
	return ""
}

var tsSQLTag = regexp.MustCompile(`(?i)(^|\.)sql$|\$(queryRaw|executeRaw)(Unsafe)?$`)

func tsSQLTagged(f *file, i int32) bool {
	p := f.parent(i)
	return p >= 0 && f.kind(i) == "template_string" && f.kind(p) == "call_expression" && tsSQLTag.MatchString(f.text(f.field(p, "function")))
}

// Test frameworks: Jest, Vitest, Mocha, Jasmine, node:test, Playwright.
var tsTestCallee = regexp.MustCompile(`^(it|test|describe|context|suite|specify|fit|fdescribe|ftest|xit|xdescribe|xtest|xcontext)((\.(skip|only|todo|fixme|concurrent|each|failing|serial|sequential))*)$`)
var tsSkipCallee = regexp.MustCompile(`^(xit|xdescribe|xtest|xcontext|fit|fdescribe|ftest)\b|\.(skip|only|fixme)\b`)

// tsTestCall returns the callee of a test or suite call (it("x", fn)), with
// .each tables unwrapped, and its title.
func tsTestCall(f *file, call int32) (callee, title string, ok bool) {
	fn := f.field(call, "function")
	if fn < 0 {
		return "", "", false
	}
	// test.each(table) is the callee of the real test call, not a test.
	if p := f.parent(call); p >= 0 && f.kind(p) == "call_expression" && f.field(p, "function") == call {
		return "", "", false
	}
	if f.kind(fn) == "call_expression" { // test.each(table)("title", fn)
		fn = f.field(fn, "function")
		if fn < 0 {
			return "", "", false
		}
	}
	callee = strings.ReplaceAll(f.text(fn), " ", "")
	if !tsTestCallee.MatchString(callee) {
		return "", "", false
	}
	args := f.named(f.field(call, "arguments"))
	if len(args) == 0 {
		return "", "", false
	}
	switch f.kind(args[0]) {
	case "string", "template_string":
		title = strings.Trim(f.text(args[0]), "'\"`")
	default:
		title = f.text(args[0])
	}
	return callee, title, true
}

// tsTestKey is the titles of the test calls that contain node i, outermost
// first, joined with " > ".
func tsTestKey(f *file, i int32) string {
	var titles []string
	for ; i >= 0; i = f.parent(i) {
		if f.kind(i) == "call_expression" {
			if _, title, ok := tsTestCall(f, i); ok {
				titles = append([]string{title}, titles...)
			}
		}
	}
	return strings.Join(titles, " > ")
}

type tsTestInfo struct {
	tests   map[string]int32 // key -> call node
	asserts map[string][]int32
	skips   []int32
}

func tsTestFacts(f *file) tsTestInfo {
	in := tsTestInfo{tests: map[string]int32{}, asserts: map[string][]int32{}}
	if f == nil {
		return in
	}
	for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
		if f.kind(i) != "call_expression" {
			continue
		}
		if callee, _, ok := tsTestCall(f, i); ok {
			in.tests[tsTestKey(f, i)] = i
			if tsSkipCallee.MatchString(callee) {
				in.skips = append(in.skips, i)
			}
			continue
		}
		fn := f.field(i, "function")
		if fn < 0 {
			continue
		}
		name := f.text(fn)
		if f.kind(fn) == "member_expression" {
			name = f.text(f.field(fn, "object"))
		}
		if name == "expect" || name == "assert" || (f.kind(fn) == "identifier" && strings.HasPrefix(name, "assert")) {
			k := tsTestKey(f, i)
			in.asserts[k] = append(in.asserts[k], i)
		}
	}
	return in
}

func (p *pair) tsTests() {
	old, new := tsTestFacts(p.old), tsTestFacts(p.new)
	for _, key := range sortedKeys(old.tests) {
		if _, ok := new.tests[key]; !ok {
			p.add(risk.TestDeleted, p.old, old.tests[key], key, fmt.Sprintf("Deletes test %q.", key))
		}
	}
	for _, key := range sortedKeys(old.asserts) {
		was, now := len(old.asserts[key]), len(new.asserts[key])
		if now >= was {
			continue
		}
		if _, kept := new.tests[key]; key != "" && !kept {
			continue // the test itself was deleted or renamed
		}
		at := old.asserts[key][0]
		for _, a := range old.asserts[key] {
			if p.old.touches(a) {
				at = a
				break
			}
		}
		where := fmt.Sprintf("test %q", key)
		if key == "" {
			where = "this file outside any test"
		}
		p.add(risk.AssertionRemoved, p.old, at, key, fmt.Sprintf("Removes %d of %d assertions in %s.", was-now, was, where))
	}
	for _, s := range new.skips {
		if p.new.startChanged(s) {
			callee, _, _ := tsTestCall(p.new, s)
			key := tsTestKey(p.new, s)
			verb := "Skips"
			if strings.Contains(callee, "only") || strings.HasPrefix(callee, "f") {
				verb = "Focuses (.only), which skips every other test in the file, on"
			}
			p.add(risk.TestSkipped, p.new, s, key, fmt.Sprintf("%s test %q (%s).", verb, key, callee))
		}
	}
}
