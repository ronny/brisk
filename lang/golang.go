package lang

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ronny/brisk/lang/internal/tswasm"
	"github.com/ronny/brisk/risk"
)

type goBackend struct{}

func (goBackend) Name() string { return "go" }

func (goBackend) Supports(path string) bool { return strings.HasSuffix(path, ".go") }

func (goBackend) Analyze(ctx context.Context, fc risk.FileChange) ([]risk.Finding, error) {
	p, err := load(ctx, fc, tswasm.Go, "go")
	if p == nil || err != nil {
		return nil, err
	}
	test := strings.HasSuffix(fc.Path, "_test.go")
	p.parseErrors()
	if !test {
		p.compareAPI(goAPI(p.old), goAPI(p.new))
	}
	if !test {
		p.accessChanges(goSymbol)
		p.authChanges(goAuthHit, goSymbol)
	}
	if p.new != nil {
		p.goSwallowed(p.new, test)
	}
	p.concurrency(goConcurrencyHit, goSymbol)
	p.sqlStrings(map[string]bool{"interpreted_string_literal": true, "raw_string_literal": true}, goSymbol, nil)
	if test {
		p.goTests()
	}
	return p.finish(), nil
}

func exported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// goReceiver returns the receiver type name and whether it is a pointer.
func goReceiver(f *file, method int32) (string, bool) {
	recv := f.field(method, "receiver")
	if recv < 0 {
		return "", false
	}
	ptr := false
	for k := recv; k < f.end[recv]; k++ {
		switch f.kind(k) {
		case "pointer_type":
			ptr = true
		case "type_identifier":
			return f.text(k), ptr
		}
	}
	return "", ptr
}

// goSymbol names the function or method that contains node i.
func goSymbol(f *file, i int32) string {
	for ; i >= 0; i = f.parent(i) {
		switch f.kind(i) {
		case "function_declaration":
			return f.text(f.field(i, "name"))
		case "method_declaration":
			return goMethodName(f, i)
		}
	}
	return ""
}

func goMethodName(f *file, i int32) string {
	t, ptr := goReceiver(f, i)
	name := f.text(f.field(i, "name"))
	if ptr {
		return "(*" + t + ")." + name
	}
	return t + "." + name
}

// sigRows returns the 1-based lines from the start of node i to the start of
// its field (such as "body").
func sigRows(f *file, i int32, field string) (int, int) {
	start, end := line(f, i)
	if b := f.field(i, field); b >= 0 {
		end = int(f.t.Nodes[b].StartRow) + 1
	}
	return start, end
}

func goAPI(f *file) map[string]decl {
	out := map[string]decl{}
	if f == nil {
		return out
	}
	put := func(key string, d decl, i int32, field string) {
		d.node = i
		d.sigStart, d.sigEnd = sigRows(f, i, field)
		out[key] = d
	}
	for _, i := range f.named(0) {
		switch f.kind(i) {
		case "function_declaration":
			if name := f.text(f.field(i, "name")); exported(name) {
				put(name, decl{name: name, what: "function", sig: f.upTo(i, "body")}, i, "body")
			}
		case "method_declaration":
			t, _ := goReceiver(f, i)
			name := f.text(f.field(i, "name"))
			if exported(t) && exported(name) {
				put(t+"."+name, decl{name: goMethodName(f, i), what: "method", sig: f.upTo(i, "body")}, i, "body")
			}
		case "type_declaration":
			for _, s := range f.named(i) {
				name := f.text(f.field(s, "name"))
				if !exported(name) {
					continue
				}
				sig := f.all(s)
				if t := f.field(s, "type"); t >= 0 && f.kind(t) == "struct_type" {
					sig = f.upTo(s, "type") + " " + goStructSig(f, t) // name, type parameters, fields
				}
				put("type "+name, decl{name: name, what: "type", sig: sig}, s, "")
			}
		case "const_declaration", "var_declaration":
			what := map[string]string{"const_declaration": "constant", "var_declaration": "variable"}[f.kind(i)]
			for k := i; k < f.end[i]; k++ {
				if kk := f.kind(k); kk != "const_spec" && kk != "var_spec" {
					continue
				}
				typ := ""
				if t := f.field(k, "type"); t >= 0 {
					typ = f.all(t)
				}
				for _, c := range f.t.Children[k] {
					if name := f.text(c); f.t.FieldName(c) == "name" && exported(name) {
						put(name, decl{name: name, what: what, sig: typ}, k, "value")
					}
				}
			}
		}
	}
	return out
}

// goStructSig is the part of a struct type that callers see: its exported
// and embedded fields, with their tags.
func goStructSig(f *file, st int32) string {
	var parts []string
	for k := st; k < f.end[st]; k++ {
		if f.kind(k) != "field_declaration" || f.parent(f.parent(k)) != st {
			continue
		}
		show := true
		for _, c := range f.t.Children[k] {
			if f.t.FieldName(c) == "name" {
				show = exported(f.text(c))
				if show {
					break
				}
			}
		}
		if show {
			parts = append(parts, f.all(k))
		}
	}
	return strings.Join(parts, "; ")
}

// goAuthHit reports whether node i is a changed condition, return, or case.
func goAuthHit(f *file, i int32) bool {
	switch f.kind(i) {
	case "if_statement":
		c := f.field(i, "condition")
		return f.startChanged(i) || (c >= 0 && f.touches(c))
	case "return_statement":
		return f.touches(i)
	case "expression_case", "type_case", "default_case":
		return f.startChanged(i)
	}
	return false
}

var (
	goErrCond = regexp.MustCompile(`\b\w*[eE]rr\w*\s*!=\s*nil\b`)
	goLogCall = regexp.MustCompile(`^(log|slog|fmt|glog|klog|zap|logrus|logger|l|lg|logging)\.\w+$|(?i)\.(log|logger)\.\w+$|\.(Print|Printf|Println|Debug|Debugf|Info|Infof|Warn|Warnf|Warning|Warningf|Error|Errorf|Log|Logf|Debugw|Infow|Warnw|Errorw)$`)
)

// goSwallowed reports errors that new code ignores: `_ = f()`, `v, _ := f()`,
// and `if err != nil` blocks that are empty, only log, or return nil.
func (p *pair) goSwallowed(f *file, test bool) {
	for i := int32(0); i < int32(len(f.t.Nodes)); i++ {
		switch f.kind(i) {
		case "assignment_statement", "short_var_declaration":
			if !f.startChanged(i) || test {
				continue
			}
			left, right := f.named(f.field(i, "left")), f.named(f.field(i, "right"))
			if len(left) == 0 || len(right) != 1 || f.kind(right[0]) != "call_expression" || f.text(left[len(left)-1]) != "_" {
				continue
			}
			callee := f.text(f.field(right[0], "function"))
			p.add(risk.ErrorSwallowed, f, i, goSymbol(f, i), fmt.Sprintf("Discards a result of %s with _, which may be an error.", short(callee)))
		case "if_statement":
			cond, body := f.field(i, "condition"), f.field(i, "consequence")
			if cond < 0 || body < 0 || !goErrCond.MatchString(f.text(cond)) || !(f.startChanged(i) || f.touches(body)) {
				continue
			}
			stmts := goStatements(f, body)
			why := ""
			switch {
			case len(stmts) == 0:
				why = "an empty block"
			case !test && allCalls(f, stmts, goLogCall):
				why = "a block that only logs"
			case len(stmts) == 1 && f.kind(stmts[0]) == "return_statement" && returnsOnlyNil(f, stmts[0]):
				why = "a block that returns nil"
			}
			if why != "" {
				p.add(risk.ErrorSwallowed, f, i, goSymbol(f, i), fmt.Sprintf("Handles %s with %s, so the error is lost.", short(f.text(cond)), why))
			}
		}
	}
}

func goStatements(f *file, block int32) []int32 {
	stmts := f.named(block)
	if len(stmts) == 1 && f.kind(stmts[0]) == "statement_list" {
		return f.named(stmts[0])
	}
	return stmts
}

// exits matches calls that end the program, so they do not lose the error.
var exits = regexp.MustCompile(`(^|\.)(Fatal|Fatalf|Fatalln|Panic|Panicf|Panicln|Exit|exit)$`)

// allCalls reports whether every statement is a call to a function whose
// name matches re and that does not end the program.
func allCalls(f *file, stmts []int32, re *regexp.Regexp) bool {
	for _, s := range stmts {
		if f.kind(s) == "expression_statement" {
			if c := f.named(s); len(c) == 1 {
				s = c[0]
			}
		}
		callee := f.text(f.field(s, "function"))
		if f.kind(s) != "call_expression" || !re.MatchString(callee) || exits.MatchString(callee) {
			return false
		}
	}
	return len(stmts) > 0
}

func returnsOnlyNil(f *file, ret int32) bool {
	n := 0
	for k := ret + 1; k < f.end[ret]; k++ {
		switch f.kind(k) {
		case "expression_list":
		case "nil":
			n++
		default:
			if f.t.Nodes[k].Named() {
				return false
			}
		}
	}
	return n > 0
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

var goSyncPkg = map[string]bool{"sync": true, "atomic": true, "errgroup": true, "semaphore": true, "singleflight": true}
var goLockMethod = map[string]bool{"Lock": true, "Unlock": true, "RLock": true, "RUnlock": true, "TryLock": true, "TryRLock": true}

func goConcurrencyWhat(f *file, i int32) string {
	switch f.kind(i) {
	case "go_statement":
		return "a goroutine"
	case "send_statement":
		return "a channel send"
	case "select_statement":
		return "a select"
	case "channel_type":
		return "a channel"
	case "unary_expression":
		if op := f.field(i, "operator"); op >= 0 && f.text(op) == "<-" {
			return "a channel receive"
		}
	case "selector_expression":
		if o := f.field(i, "operand"); o >= 0 && goSyncPkg[f.text(o)] {
			return f.text(o) + " use"
		}
		if goLockMethod[f.text(f.field(i, "field"))] {
			return "locking"
		}
	case "qualified_type":
		if pk := f.field(i, "package"); pk >= 0 && goSyncPkg[f.text(pk)] {
			return f.text(i)
		}
	}
	return ""
}

func goConcurrencyHit(f *file, i int32) string {
	if !f.startChanged(i) {
		return ""
	}
	return goConcurrencyWhat(f, i)
}

var goTestName = regexp.MustCompile(`^(Test|Benchmark|Fuzz|Example)`)

type goTestInfo struct {
	tests      map[string]int32 // test or subtest key -> node
	asserts    map[string][]int32
	skips      []int32
	topByIndex map[int32]string
}

func goTestFacts(f *file) goTestInfo {
	in := goTestInfo{tests: map[string]int32{}, asserts: map[string][]int32{}}
	if f == nil {
		return in
	}
	for _, fn := range f.named(0) {
		if f.kind(fn) != "function_declaration" && f.kind(fn) != "method_declaration" {
			continue
		}
		top := goSymbol(f, fn)
		if f.kind(fn) == "function_declaration" && goTestName.MatchString(top) {
			in.tests[top] = fn
		}
		for k := fn; k < f.end[fn]; k++ {
			if f.kind(k) != "call_expression" {
				continue
			}
			callee := f.field(k, "function")
			if callee < 0 || f.kind(callee) != "selector_expression" {
				continue
			}
			operand, method := f.text(f.field(callee, "operand")), f.text(f.field(callee, "field"))
			switch {
			case goAssertion(operand, method):
				in.asserts[top] = append(in.asserts[top], k)
			case method == "Skip" || method == "Skipf" || method == "SkipNow":
				in.skips = append(in.skips, k)
			case method == "Run":
				if args := f.named(f.field(k, "arguments")); len(args) > 0 && strings.HasSuffix(f.kind(args[0]), "string_literal") {
					in.tests[top+"/"+strings.Trim(f.text(args[0]), "\"`")] = k
				}
			}
		}
	}
	return in
}

var goTestVar = map[string]bool{"t": true, "b": true, "tb": true, "f": true, "c": true, "s": true}
var goFailMethod = map[string]bool{"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true, "Fail": true, "FailNow": true, "Assert": true, "Check": true}

func goAssertion(operand, method string) bool {
	switch {
	case goTestVar[operand] && goFailMethod[method]:
		return true
	case operand == "assert" || operand == "require" || operand == "is" || operand == "qt" || operand == "be":
		return true
	case strings.HasSuffix(operand, ".Require()") || strings.HasSuffix(operand, ".Assert()"):
		return true
	}
	return false
}

func (p *pair) goTests() {
	old, new := goTestFacts(p.old), goTestFacts(p.new)
	for _, name := range sortedKeys(old.tests) {
		if _, ok := new.tests[name]; !ok {
			p.add(risk.TestDeleted, p.old, old.tests[name], name, fmt.Sprintf("Deletes test %s.", name))
		}
	}
	for _, fn := range sortedKeys(old.asserts) {
		was, now := len(old.asserts[fn]), len(new.asserts[fn])
		if _, kept := new.tests[fn]; now >= was || (!kept && goTestName.MatchString(fn)) {
			continue
		}
		if p.new != nil && !hasFunc(new, fn, p.new) {
			continue
		}
		at := old.asserts[fn][0]
		for _, a := range old.asserts[fn] {
			if p.old.touches(a) {
				at = a
				break
			}
		}
		p.add(risk.AssertionRemoved, p.old, at, fn, fmt.Sprintf("Removes %d of %d assertions in %s.", was-now, was, fn))
	}
	for _, s := range new.skips {
		if p.new.startChanged(s) {
			sym := goSymbol(p.new, s)
			p.add(risk.TestSkipped, p.new, s, sym, fmt.Sprintf("Skips %s.", orFile(sym)))
		}
	}
}

// hasFunc reports whether fn still exists in the new file, so assertions
// that left with a deleted helper are not counted twice.
func hasFunc(in goTestInfo, fn string, f *file) bool {
	if _, ok := in.asserts[fn]; ok {
		return true
	}
	for _, c := range f.named(0) {
		if k := f.kind(c); (k == "function_declaration" || k == "method_declaration") && goSymbol(f, c) == fn {
			return true
		}
	}
	return false
}
