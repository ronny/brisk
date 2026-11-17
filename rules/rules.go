// Package rules applies deterministic path, diff, structural, and session
// rules to a change. The highest tier among the hits is the floor: the
// model can raise the tier above it but never lower it, so text inside the
// diff cannot talk brisk down.
package rules

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
)

// Hit is one rule that matched.
type Hit struct {
	ID       string
	Tier     risk.Tier
	Text     string
	Source   string // "rule", "language", or "session"
	Evidence []risk.Evidence
}

// Floor returns the highest tier among hits, or Low.
func Floor(hits []Hit) risk.Tier {
	t := risk.Low
	for _, h := range hits {
		t = risk.Max(t, h.Tier)
	}
	return t
}

// pathRule matches changed paths.
type pathRule struct {
	id     string
	tier   risk.Tier
	text   string
	match  func(p string) bool
	ignore func(fc risk.FileChange) bool
}

// lineRule matches added lines.
type lineRule struct {
	id    string
	tier  risk.Tier
	text  string
	re    *regexp.Regexp
	paths func(p string) bool // nil means every non-test, non-doc file
	// inComments makes the rule match comment lines too: a secret is
	// leaked whether or not it sits in a comment.
	inComments bool
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// segs matches whole path segments, splitting camelCase too, so
// "authService.ts" matches "auth" but "author.go" does not.
func segs(names ...string) func(string) bool {
	re := regexp.MustCompile(`(?i)(^|[/_.-])(` + strings.Join(names, "|") + `)([/_.-]|$)`)
	return func(p string) bool { return re.MatchString(camelBoundary.ReplaceAllString(p, "${1}_${2}")) }
}

func globs(patterns ...string) func(string) bool {
	return func(p string) bool {
		for _, g := range patterns {
			if ok, _ := doublestar.Match(g, p); ok {
				return true
			}
		}
		return false
	}
}

func anyOf(fs ...func(string) bool) func(string) bool {
	return func(p string) bool {
		for _, f := range fs {
			if f(p) {
				return true
			}
		}
		return false
	}
}

// IsTest reports whether p looks like a test file.
var testFile = regexp.MustCompile(`\.(test|spec)\.[cm]?[jt]sx?$`)

func IsTest(p string) bool {
	b := path.Base(p)
	return strings.HasSuffix(b, "_test.go") || testFile.MatchString(b) ||
		strings.Contains(p, "/__tests__/") || strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/") ||
		strings.Contains(p, "/e2e/") || strings.HasPrefix(p, "e2e/")
}

// IsDoc reports whether p is documentation.
func IsDoc(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".mdx", ".txt", ".rst", ".adoc":
		return true
	}
	return strings.HasPrefix(p, "docs/")
}

// IsGenerated reports whether p is a lockfile or generated code.
func IsGenerated(p string) bool {
	b := path.Base(p)
	switch b {
	case "go.sum", "pnpm-lock.yaml", "package-lock.json", "yarn.lock", "bun.lockb", "bun.lock", "mise.lock", "Cargo.lock", "buf.lock", "uv.lock", "poetry.lock":
		return true
	}
	return strings.Contains(b, ".pb.") || strings.HasSuffix(b, "_pb.ts") || strings.HasSuffix(b, "_pb.js") ||
		strings.HasSuffix(b, ".gen.go") || strings.HasSuffix(b, "_gen.go") || strings.HasSuffix(b, ".generated.ts") ||
		strings.Contains(p, "/gen/") || strings.HasPrefix(p, "gen/") || strings.HasSuffix(b, ".snap") || strings.HasSuffix(b, ".min.js")
}

// secretRe matches values that look like secrets.
var secretRe = regexp.MustCompile(`(AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|\bsk-(live|proj|or-v1)?-?[A-Za-z0-9]{24,}|\bgh[pousr]_[A-Za-z0-9]{36}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bsk_live_[A-Za-z0-9]{16,})`)

// RedactSecrets replaces each value the secret_added rule would match
// with "[redacted secret]".
func RedactSecrets(s string) string {
	return secretRe.ReplaceAllString(s, "[redacted secret]")
}

// credentialFiles are files whose values are all secrets, such as .env
// files and keys; secrets_config also matches code about secrets.
var credentialFiles = globs("**/.env", "**/.env.*", "**/fnox.toml", "**/*.pem", "**/*.key", "**/secrets.y*ml", "**/*.secrets.*")

// IsCredentialFile reports whether p holds credentials, so its contents
// should not leave the machine.
func IsCredentialFile(p string) bool { return credentialFiles(p) }

// IsGeneratedChange reports whether fc touches only generated code: its
// path is generated, and so was its old path when it was renamed or copied.
// Renaming code into a generated path must not exempt it from analysis.
func IsGeneratedChange(fc risk.FileChange) bool {
	return IsGenerated(fc.Path) && (fc.OldPath == "" || IsGenerated(fc.OldPath))
}

func codeOnly(fc risk.FileChange) bool {
	return !IsTest(fc.Path) && !IsDoc(fc.Path) && !IsGeneratedChange(fc)
}

var builtinPathRules = []pathRule{
	{
		id: "migration", tier: risk.Medium, text: "Changes a database migration",
		match: anyOf(segs("migrations?", "migrate"), globs("**/schema.sql", "**/*.up.sql", "**/*.down.sql")),
	},
	{
		id: "auth", tier: risk.Medium, text: "Changes authentication or authorisation code",
		match:  segs("auth", "authn", "authz", "oauth", "oidc", "permissions?", "rbac", "acl", "access_?control", "login", "jwt", "passwords?", "csrf", "sessions?", "middleware", "policy", "policies", "guard", "guards", "roles?"),
		ignore: func(fc risk.FileChange) bool { return !codeOnly(fc) },
	},
	{
		id: "payments", tier: risk.Medium, text: "Changes payment or billing code",
		match:  segs("billing", "payments?", "invoices?", "checkout", "stripe", "refunds?", "charges?", "subscriptions?", "pricing"),
		ignore: func(fc risk.FileChange) bool { return !codeOnly(fc) },
	},
	{
		id: "ci_deploy", tier: risk.Medium, text: "Changes CI, deployment, or infrastructure",
		match: anyOf(globs(".github/workflows/**", ".buildkite/**", ".circleci/**", ".gitlab-ci.yml", "**/Dockerfile", "**/Dockerfile.*", "**/docker-compose*.y*ml", "**/*.tf", "**/*.tfvars", "**/k8s/**", "**/helm/**", "**/kustomization.y*ml", "**/fly.toml", "**/Procfile", "**/wrangler.toml", "**/wrangler.json*"),
			segs("deploy", "deployment", "infra", "terraform", "pulumi")),
	},
	{
		id: "dependencies", tier: risk.Medium, text: "Changes dependencies",
		match: globs("**/go.mod", "**/package.json", "**/Cargo.toml", "**/pyproject.toml", "**/requirements*.txt", "**/Gemfile"),
		ignore: func(fc risk.FileChange) bool {
			// package.json edits that only touch scripts are common and harmless.
			if path.Base(fc.Path) != "package.json" {
				return false
			}
			for _, l := range append(gitx.AddedLines(fc), gitx.RemovedLines(fc)...) {
				if depLine.MatchString(l.Text) {
					return false
				}
			}
			return true
		},
	},
	{
		id: "secrets_config", tier: risk.Medium, text: "Changes secret or credential configuration",
		match:  anyOf(credentialFiles, segs("secrets?", "credentials?")),
		ignore: func(fc risk.FileChange) bool { return IsDoc(fc.Path) || IsTest(fc.Path) },
	},
	{
		id: "brisk_config", tier: risk.Medium, text: "Changes brisk's own risk rules",
		match: globs(".brisk.yaml"),
	},
	{
		id: "public_api", tier: risk.Medium, text: "Changes a public API definition",
		match: globs("**/*.proto", "**/openapi*.y*ml", "**/openapi*.json", "**/swagger*.y*ml", "**/*.graphql", "**/schema.graphqls"),
	},
}

// sqlBearing matches files where SQL text is code rather than prose or CSS.
var sqlBearing = regexp.MustCompile(`\.(sql|go|py|rb|java|kt|rs|ts|js|mjs|cjs|php|cs)$`)

var depLine = regexp.MustCompile(`^\s*"(@[\w.-]+/)?[\w.-]+"\s*:\s*"[~^<>=]*\d`)

var builtinLineRules = []lineRule{
	{
		id: "destructive_sql", tier: risk.High, text: "Adds destructive SQL",
		re: regexp.MustCompile(`(?i:\bdrop\s+(table|column|schema|database|index|view|type)\b)|(?i:\balter\s+table\s+\S+\s+drop\b)|(?i:\btruncate\s+table\s+\w)|\bTRUNCATE\s+["\w]|(?i:\bdelete\s+from\s+["\w.]+\s*(;|$|["'` + "`" + `]))`),
		paths: func(p string) bool {
			// A down migration undoes its up migration; dropping what
			// the up file created is expected.
			return !IsTest(p) && sqlBearing.MatchString(p) && !strings.HasSuffix(p, ".down.sql")
		},
	},
	{
		id: "secret_added", tier: risk.High, text: "Adds what looks like a secret", inComments: true,
		re:    secretRe,
		paths: func(p string) bool { return !IsDoc(p) },
	},
	{
		id: "tls_disabled", tier: risk.High, text: "Turns off TLS certificate checks", inComments: true,
		re: regexp.MustCompile(`(InsecureSkipVerify:\s*true|rejectUnauthorized:\s*false|NODE_TLS_REJECT_UNAUTHORIZED\s*=?\s*['"]?0|verify\s*=\s*False)`),
	},
	{
		id: "test_disabled", tier: risk.Medium, text: "Skips or focuses tests",
		re:    regexp.MustCompile(`(\bt\.Skip(f|Now)?\(|\b(it|describe|test)\.(skip|only)\(|\bx(it|describe)\(|\bf(it|describe)\()`),
		paths: IsTest,
	},
}

// Config adds project rules, e.g. from .brisk.yaml.
type Config struct {
	PathRules []risk.PathRule
}

// Apply returns every rule hit for the change, the session, and the
// language findings. Hits are sorted by tier, then ID.
func Apply(cs *risk.ChangeSet, findings []risk.Finding, session *risk.Session, cfg Config) []Hit {
	var hits []Hit
	add := func(id string, tier risk.Tier, text, source string, ev risk.Evidence) {
		for i := range hits {
			if hits[i].ID == id {
				if len(hits[i].Evidence) < 5 {
					hits[i].Evidence = append(hits[i].Evidence, ev)
				}
				hits[i].Tier = risk.Max(hits[i].Tier, tier)
				return
			}
		}
		hits = append(hits, Hit{ID: id, Tier: tier, Text: text, Source: source, Evidence: []risk.Evidence{ev}})
	}

	for _, fc := range cs.Files {
		paths := []string{fc.Path}
		if fc.OldPath != "" {
			paths = append(paths, fc.OldPath)
		}
		for _, r := range builtinPathRules {
			if r.ignore != nil && r.ignore(fc) {
				continue
			}
			for _, p := range paths {
				if r.match(p) {
					add(r.id, r.tier, r.text, "rule", risk.Evidence{Path: fc.Path})
					break
				}
			}
		}
		for _, r := range cfg.PathRules {
			for _, p := range paths {
				if globs(r.Paths...)(p) {
					text := r.Reason
					if text == "" {
						text = "Changes " + r.ID
					}
					add("config:"+r.ID, r.Floor, text, "rule", risk.Evidence{Path: fc.Path})
					break
				}
			}
		}
		if fc.Binary {
			continue
		}
		added := gitx.AddedLines(fc)
		for _, r := range builtinLineRules {
			if r.paths != nil && !r.paths(fc.Path) {
				continue
			}
			if r.paths == nil && !codeOnly(fc) {
				continue
			}
			for _, l := range added {
				if r.re.MatchString(l.Text) && (r.inComments || !isComment(fc.Path, l.Text)) {
					add(r.id, r.tier, r.text, "rule", risk.Evidence{Path: fc.Path, StartLine: l.N, EndLine: l.N})
				}
			}
		}
	}

	for _, fc := range cs.Files {
		if strings.HasSuffix(fc.Path, ".proto") {
			for _, ev := range protoRenumbered(fc) {
				add("proto_renumbered", risk.High, "Renumbers or reuses a protobuf field number", "rule", ev)
			}
		}
	}

	for _, f := range findings {
		tier, ok := FindingTier[f.Kind]
		if !ok || f.Kind == risk.ParseError {
			continue
		}
		add("lang:"+string(f.Kind), tier, findingText[f.Kind], "language",
			risk.Evidence{Path: f.Path, StartLine: f.StartLine, EndLine: f.EndLine, Symbol: f.Symbol})
	}

	if session != nil {
		if c, ok := lastVerification(session); ok && c.ExitCode != nil && *c.ExitCode != 0 {
			add("session:verification_failed", risk.Medium,
				fmt.Sprintf("The last check the agent ran failed: %s", truncate(RedactSecrets(c.Command), 80)), "session", risk.Evidence{})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Tier != hits[j].Tier {
			return hits[i].Tier > hits[j].Tier
		}
		return hits[i].ID < hits[j].ID
	})
	return hits
}

var (
	protoField   = regexp.MustCompile(`^\s*(?:repeated\s+|optional\s+|required\s+)?([\w.]+(?:<[\w., ]+>)?)\s+(\w+)\s*=\s*(\d+)\s*[;\[]`)
	protoMessage = regexp.MustCompile(`^\s*message\s+(\w+)\s*(\{)?`)
)

type protoFieldInfo struct {
	num, typ string
	line     int
}

// protoCode blanks out comments and string literals, keeping line breaks,
// so braces and "//" inside them do not count.
func protoCode(src []byte) []string {
	var b strings.Builder
	inBlock, inStr := false, byte(0)
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			b.WriteByte(c)
			if inStr != 0 {
				inStr = 0 // unterminated string: stop at the line end
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
			b.WriteByte(' ')
		case inStr != 0:
			if c == '\\' {
				i++
			} else if c == inStr {
				inStr = 0
			}
			b.WriteByte(' ')
		case c == '"' || c == '\'':
			inStr = c
			b.WriteByte(' ')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
			b.WriteString("  ")
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i--
		default:
			b.WriteByte(c)
		}
	}
	return strings.Split(b.String(), "\n")
}

// protoFields maps "Message.Nested" to its fields by name. Fields inside a
// oneof belong to the enclosing message. One field per line, as protoc
// style has it.
func protoFields(src []byte) map[string]map[string]protoFieldInfo {
	out := map[string]map[string]protoFieldInfo{}
	var stack []string // message name, or "" for any other block
	pending := ""      // a message whose "{" is on a later line
	for i, line := range protoCode(src) {
		if m := protoField.FindStringSubmatch(line); m != nil {
			var path []string
			for _, s := range stack {
				if s != "" {
					path = append(path, s)
				}
			}
			msg := strings.Join(path, ".")
			if out[msg] == nil {
				out[msg] = map[string]protoFieldInfo{}
			}
			out[msg][m[2]] = protoFieldInfo{num: m[3], typ: m[1], line: i + 1}
		}
		// Every brace opens or closes a block, including those in field
		// options that span lines; a message's first brace opens it.
		opens := strings.Count(line, "{")
		if m := protoMessage.FindStringSubmatch(line); m != nil {
			pending = m[1]
		}
		if pending != "" && opens > 0 {
			stack = append(stack, pending)
			pending = ""
			opens--
		}
		for ; opens > 0; opens-- {
			stack = append(stack, "")
		}
		for closes := strings.Count(line, "}"); closes > 0 && len(stack) > 0; closes-- {
			stack = stack[:len(stack)-1]
		}
	}
	return out
}

// protoWire groups scalar types that share a wire encoding, following the
// protobuf language guide's compatibility rules. Other names are messages
// or enums, which brisk cannot compare.
var protoWire = map[string]string{
	"int32": "varint", "uint32": "varint", "int64": "varint", "uint64": "varint", "bool": "varint",
	"sint32": "zigzag", "sint64": "zigzag",
	"string": "bytes", "bytes": "bytes",
	"fixed32": "fixed32", "sfixed32": "fixed32", "float": "float",
	"fixed64": "fixed64", "sfixed64": "fixed64", "double": "double",
}

// protoCompatible reports whether a field can change from type a to b
// without breaking the wire format, as far as brisk can tell.
func protoCompatible(a, b string) bool {
	wa, sa := protoWire[a]
	wb, sb := protoWire[b]
	switch {
	case a == b:
		return true
	case sa && sb:
		return wa == wb
	case !sa && !sb:
		return true // two message or enum types: may be a rename
	}
	return false
}

// protoRenumbered compares each message's fields before and after. A field
// whose number or wire type changed, or a number now used by a field of an
// incompatible type, breaks every client and stored message. A field
// renamed with the same number and a compatible type is not reported.
func protoRenumbered(fc risk.FileChange) []risk.Evidence {
	if len(fc.Old) == 0 || len(fc.New) == 0 {
		return nil
	}
	before, after := protoFields(fc.Old), protoFields(fc.New)
	var out []risk.Evidence
	for msg, fields := range after {
		old, ok := before[msg]
		if !ok {
			continue
		}
		byNum := map[string]protoFieldInfo{}
		for _, f := range old {
			byNum[f.num] = f
		}
		for name, f := range fields {
			o, same := old[name]
			prev, reused := byNum[f.num]
			if same && (o.num != f.num || !protoCompatible(o.typ, f.typ)) ||
				!same && reused && !protoCompatible(prev.typ, f.typ) {
				out = append(out, risk.Evidence{Path: fc.Path, StartLine: f.line, EndLine: f.line, Symbol: msg + "." + name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartLine < out[j].StartLine })
	return out
}

// FindingTier is the floor each kind of structural finding sets. Kinds that
// are not listed are evidence for the model only.
var FindingTier = map[risk.FindingKind]risk.Tier{
	risk.AuthLogicChanged:         risk.High,
	risk.ExportedSymbolRemoved:    risk.Medium,
	risk.AssertionRemoved:         risk.Medium,
	risk.TestSkipped:              risk.Medium,
	risk.TestDeleted:              risk.Medium,
	risk.ErrorSwallowed:           risk.Medium,
	risk.ExportedSignatureChanged: risk.Low,
	risk.ConcurrencyChanged:       risk.Low,
	risk.SQLChanged:               risk.Low,
}

var findingText = map[risk.FindingKind]string{
	risk.AuthLogicChanged:         "Changes logic inside an access check",
	risk.ExportedSymbolRemoved:    "Removes or renames an exported symbol",
	risk.ExportedSignatureChanged: "Changes an exported signature",
	risk.AssertionRemoved:         "Removes or weakens test assertions",
	risk.TestSkipped:              "Skips or focuses tests",
	risk.TestDeleted:              "Deletes tests",
	risk.ErrorSwallowed:           "Ignores an error",
	risk.ConcurrencyChanged:       "Changes concurrent code",
	risk.SQLChanged:               "Changes SQL queries",
}

var verifyRe = regexp.MustCompile(`(?i)(\bgo (test|vet|build)\b|golangci-lint|\b(npm|pnpm|yarn|bun)( run)? (test|typecheck|tsc|lint|check|build)\b|\btsc\b|\bvitest\b|\bjest\b|\bplaywright\b|\bpytest\b|\bcargo (test|check|clippy)\b|\bmise (run )?(test|check|lint|ci)\b|\bmake (test|check|lint)\b)`)

// IsVerification reports whether a shell command runs tests, type checks,
// lint, or a build.
func IsVerification(cmd string) bool { return verifyRe.MatchString(cmd) }

func lastVerification(s *risk.Session) (risk.Command, bool) {
	for i := len(s.Commands) - 1; i >= 0; i-- {
		if IsVerification(s.Commands[i].Command) {
			return s.Commands[i], true
		}
	}
	return risk.Command{}, false
}

// isComment reports whether line is a whole-line comment in the language of
// path. A line that has code after a block comment closes is not.
func isComment(p, line string) bool {
	t := strings.TrimSpace(line)
	switch strings.ToLower(path.Ext(p)) {
	case ".sql":
		return strings.HasPrefix(t, "--")
	case ".py", ".rb", ".sh", ".yaml", ".yml", ".toml":
		return strings.HasPrefix(t, "#")
	}
	if strings.HasPrefix(t, "//") {
		return true
	}
	if strings.HasPrefix(t, "/*") {
		end := strings.Index(t, "*/")
		return end < 0 || strings.TrimSpace(t[end+2:]) == ""
	}
	// A "* " continuation line of a block comment, not a pointer write.
	return t == "*" || strings.HasPrefix(t, "* ") && !strings.ContainsAny(t, "=(")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
