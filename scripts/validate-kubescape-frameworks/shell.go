package main

// SPIKE (#3564): the shell-reading layer, backed by mvdan.cc/sh/v3/syntax.
//
// Everything the hand-written lexer tracked line by line — quote state carried across
// physical lines, backslash-newline continuation, comment boundaries, heredoc bodies and
// their terminators, `&&`/`||`/`|`/`&` segmentation, substitutions spanning lines — is a
// property of the parse tree here. The guard's RULES are unchanged; they now read parsed
// words instead of re-lexed tokens.

import (
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ghaExpression matches a GitHub Actions `${{ … }}` expression. The runner substitutes it
// TEXTUALLY before any shell starts, so to the shell it is unknown text — and bash syntax
// cannot even parse it (`${{` is an invalid parameter name). Each one is replaced by a
// parameter expansion, which every rule here already treats as undecidable run-time text.
var ghaExpression = regexp.MustCompile(`(?s)\$\{\{.*?\}\}`)

const ghaPlaceholder = "${GITHUB_EXPRESSION}"

func newShellParser() *syntax.Parser {
	return syntax.NewParser(syntax.Variant(syntax.LangBash))
}

// shWord is one shell word, read from the parse tree.
type shWord struct {
	raw          string // the word as written, continuations removed
	value        string // quotes and escapes resolved; an expanding part is kept as written
	expands      bool   // carries a run-time expansion: parameter, substitution, $'…', $"…", or an unquoted pattern/brace
	spelled      bool   // written with quoting or escaping, so raw and value may differ
	fullyQuoted  bool   // exactly one '…' or "…" part
	doubleQuoted bool   // exactly one plain "…" part: cannot split into more words
	line         uint
}

func slice(src string, n syntax.Node) string { return src[n.Pos().Offset():n.End().Offset()] }

func newWord(w *syntax.Word, src string) shWord {
	sw := shWord{line: w.Pos().Line()}
	if len(w.Parts) == 1 {
		switch p := w.Parts[0].(type) {
		case *syntax.SglQuoted:
			sw.fullyQuoted = !p.Dollar
		case *syntax.DblQuoted:
			sw.fullyQuoted = !p.Dollar
			sw.doubleQuoted = !p.Dollar
		}
	}
	var raw, val strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			raw.WriteString(p.Value)
			v, expands, escaped := unquotedLiteral(p.Value)
			val.WriteString(v)
			sw.expands = sw.expands || expands
			sw.spelled = sw.spelled || escaped
		case *syntax.SglQuoted:
			sw.spelled = true
			text := slice(src, p)
			raw.WriteString(text)
			if p.Dollar {
				sw.expands = true
				val.WriteString(text)
			} else {
				val.WriteString(p.Value)
			}
		case *syntax.DblQuoted:
			sw.spelled = true
			text := slice(src, p)
			raw.WriteString(text)
			if p.Dollar {
				sw.expands = true
				val.WriteString(text)
				continue
			}
			for _, inner := range p.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					val.WriteString(doubleQuotedLiteral(lit.Value))
					continue
				}
				sw.expands = true
				val.WriteString(slice(src, inner))
			}
		default: // ParamExp, CmdSubst, ArithmExp, ProcSubst, ExtGlob
			sw.expands = true
			text := slice(src, part)
			raw.WriteString(text)
			val.WriteString(text)
		}
	}
	sw.raw, sw.value = raw.String(), val.String()
	return sw
}

// unquotedLiteral resolves the backslash escapes the parser leaves in an unquoted literal
// and reports whether an unescaped pattern or brace character makes it expand.
func unquotedLiteral(s string) (value string, expands, escaped bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			escaped = true
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
			continue
		case '$', '`', '*', '?', '[', '{':
			expands = true
		}
		b.WriteByte(c)
	}
	return b.String(), expands, escaped
}

// doubleQuotedLiteral resolves the four escapes bash honours inside double quotes.
func doubleQuotedLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func assignWord(as *syntax.Assign, src string) shWord {
	sw := shWord{raw: slice(src, as), line: as.Pos().Line()}
	if as.Name != nil && as.Array == nil && as.Index == nil && !as.Append && !as.Naked {
		sw.value = as.Name.Value + "="
		if as.Value != nil {
			v := newWord(as.Value, src)
			sw.value += v.value
			sw.expands, sw.spelled = v.expands, v.spelled
		}
		return sw
	}
	sw.value = sw.raw
	syntax.Walk(as, func(n syntax.Node) bool {
		if w, ok := n.(*syntax.Word); ok && newWord(w, src).expands {
			sw.expands = true
		}
		return true
	})
	return sw
}

// callSite is one simple command, with what the parse tree says about whether it runs
// and whether its exit status reaches the step.
type callSite struct {
	fields      []shWord
	nested      string // "" at top level; otherwise the construct enclosing the command
	conditional bool   // right-hand side of `&&` / `||`
	masked      bool   // status discarded: `&`, a non-final pipeline stage, `!`, or an `||` whose right side does not re-raise
	top         int    // index of the enclosing top-level statement
	pos         uint
}

type scriptAnalysis struct {
	src       string
	sites     []callSite
	compound  string   // the first compound command anywhere in the script
	multiline string   // a command/process substitution that spans lines
	heredocs  []string // heredoc bodies, for the loose conditional screen
}

type walkCtx struct {
	nested      string
	conditional bool
	masked      bool
	top         int
}

// analyseScript parses one `run:` scalar and records every simple command in it.
func analyseScript(scalar string) (*scriptAnalysis, error) {
	src := ghaExpression.ReplaceAllLiteralString(scalar, ghaPlaceholder)
	f, err := newShellParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, err
	}
	a := &scriptAnalysis{src: src}
	for i, s := range f.Stmts {
		a.stmt(s, walkCtx{top: i})
	}
	sort.SliceStable(a.sites, func(i, j int) bool { return a.sites[i].pos < a.sites[j].pos })
	return a, nil
}

func (a *scriptAnalysis) stmt(s *syntax.Stmt, ctx walkCtx) {
	if s == nil {
		return
	}
	// `!` inverts the status and, under `bash -e`, a negated command never fails the step.
	if s.Background || s.Coprocess || s.Negated {
		ctx.masked = true
	}
	for _, r := range s.Redirs {
		a.nestedIn(r, ctx, "a redirection")
		if r.Hdoc != nil {
			a.heredocs = append(a.heredocs, heredocBody(a.src, r.Hdoc))
		}
	}
	a.command(s.Cmd, ctx)
}

func (a *scriptAnalysis) command(cmd syntax.Command, ctx walkCtx) {
	switch c := cmd.(type) {
	case nil:
	case *syntax.CallExpr:
		fields := make([]shWord, 0, len(c.Assigns)+len(c.Args))
		for _, as := range c.Assigns {
			fields = append(fields, assignWord(as, a.src))
		}
		for _, w := range c.Args {
			fields = append(fields, newWord(w, a.src))
		}
		a.site(fields, ctx, c.Pos().Offset())
		a.nestedIn(c, ctx, "a command substitution")
	case *syntax.DeclClause:
		fields := []shWord{{raw: c.Variant.Value, value: c.Variant.Value, line: c.Pos().Line()}}
		for _, as := range c.Args {
			fields = append(fields, assignWord(as, a.src))
		}
		a.site(fields, ctx, c.Pos().Offset())
		a.nestedIn(c, ctx, "a command substitution")
	case *syntax.BinaryCmd:
		x, y := ctx, ctx
		y.conditional = true
		switch c.Op {
		case syntax.AndStmt:
		case syntax.OrStmt:
			// `a || b` reports b's status when a fails, so a's failure survives only when b
			// is certain to fail and its own status reaches the step.
			if !a.reRaises(c.Y) {
				x.masked = true
			}
		default: // `|` and `|&`: only the last stage's status survives without pipefail
			x.masked = true
			y.conditional = ctx.conditional
		}
		a.stmt(c.X, x)
		a.stmt(c.Y, y)
	case *syntax.TimeClause:
		// Not a compound command (it changes no reachability), but a prefix: a scan under
		// it is refused like any other prefixed invocation.
		t := ctx
		t.nested = "`time`"
		a.stmt(c.Stmt, t)
	case *syntax.TestClause, *syntax.LetClause:
		a.nestedIn(c, ctx, "a test")
	default: // Block, Subshell, IfClause, WhileClause, ForClause, CaseClause, FuncDecl, CoprocClause, ArithmCmd
		name := compoundName(c)
		if a.compound == "" {
			a.compound = name
		}
		a.nestedIn(c, ctx, "the compound command "+name)
	}
}

func (a *scriptAnalysis) site(fields []shWord, ctx walkCtx, pos uint) {
	a.sites = append(a.sites, callSite{
		fields: fields, nested: ctx.nested, conditional: ctx.conditional,
		masked: ctx.masked, top: ctx.top, pos: pos,
	})
}

// nestedIn records every command inside n — statements of a compound body, and command or
// process substitutions in its words — as NESTED: whether it runs is not a fact about the
// top-level command sequence.
func (a *scriptAnalysis) nestedIn(n syntax.Node, ctx walkCtx, label string) {
	inner := ctx
	if inner.nested == "" {
		inner.nested = label
	}
	syntax.Walk(n, func(m syntax.Node) bool {
		if m == n {
			return true
		}
		switch x := m.(type) {
		case *syntax.Stmt:
			a.stmt(x, inner)
			return false
		case *syntax.CmdSubst:
			if x.Left.Line() != x.Right.Line() && a.multiline == "" {
				a.multiline = "$(...)"
				if x.Backquotes {
					a.multiline = "backtick"
				}
			}
			sub := ctx
			if sub.nested == "" {
				sub.nested = "a command substitution"
			}
			for _, s := range x.Stmts {
				a.stmt(s, sub)
			}
			return false
		case *syntax.ProcSubst:
			if x.OpPos.Line() != x.Rparen.Line() && a.multiline == "" {
				a.multiline = "<(...)"
			}
			sub := ctx
			if sub.nested == "" {
				sub.nested = "a process substitution"
			}
			for _, s := range x.Stmts {
				a.stmt(s, sub)
			}
			return false
		}
		return true
	})
}

// reRaises reports whether the right side of `||` is certain to fail and reach the step.
func (a *scriptAnalysis) reRaises(s *syntax.Stmt) bool {
	if s == nil || s.Background || s.Negated || s.Coprocess {
		return false
	}
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Assigns) > 0 {
		return false
	}
	words := make([]string, 0, len(c.Args))
	for _, w := range c.Args {
		sw := newWord(w, a.src)
		if sw.expands || sw.spelled {
			return false
		}
		words = append(words, sw.value)
	}
	return provablyFails(strings.Join(words, " "))
}

func compoundName(c syntax.Command) string {
	switch x := c.(type) {
	case *syntax.IfClause:
		return "if"
	case *syntax.WhileClause:
		if x.Until {
			return "until"
		}
		return "while"
	case *syntax.ForClause:
		if x.Select {
			return "select"
		}
		return "for"
	case *syntax.CaseClause:
		return "case"
	case *syntax.Block:
		return "{"
	case *syntax.Subshell:
		return "("
	case *syntax.FuncDecl:
		return "function"
	case *syntax.CoprocClause:
		return "coproc"
	case *syntax.ArithmCmd:
		return "(("
	}
	return "compound command"
}

// heredocBody returns a heredoc's body text without its terminator line.
func heredocBody(src string, w *syntax.Word) string {
	if len(w.Parts) == 0 {
		return ""
	}
	body := src[w.Parts[0].Pos().Offset():w.Parts[len(w.Parts)-1].End().Offset()]
	if i := strings.LastIndexByte(body, '\n'); i >= 0 {
		return body[:i+1]
	}
	return ""
}

func rawText(fields []shWord) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f.raw
	}
	return strings.Join(parts, " ")
}

// framed reports whether a command carries `--framework`, as written or once resolved.
func framed(fields []shWord) bool {
	for _, f := range fields {
		if strings.Contains(f.raw, "--framework") || strings.Contains(f.value, "--framework") {
			return true
		}
	}
	return false
}

func scanCommand(fields []shWord) bool {
	return len(fields) >= 3 && fields[0].value == "ksail" && fields[1].value == "workload" && fields[2].value == "scan"
}

// scanCandidate reports whether a `run:` scalar can invoke the scan at all. Deliberately
// LOOSE: it decides only whether a conditional step is worth refusing, and a false positive
// there costs a diagnosable refusal while a false negative reopens the hole.
func scanCandidate(scalar string) bool {
	return scanCandidateDepth(scalar, 0)
}

func scanCandidateDepth(scalar string, depth int) bool {
	a, err := analyseScript(scalar)
	if err != nil {
		// Unreadable shell: fail toward refusing whenever it names a scan word at all.
		return strings.Contains(scalar, "ksail") || strings.Contains(scalar, "workload") || strings.Contains(scalar, "scan")
	}
	framedScalar := strings.Contains(scalar, "--framework")
	// Per command, then per top-level statement and per physical line — the groupings the
	// line-oriented predecessor read, so `echo workload scan … | xargs ksail` stays refused.
	groups := map[[2]int][]shWord{}
	var all []shWord
	for _, s := range a.sites {
		if looseCandidate(s.fields, framedScalar) {
			return true
		}
		groups[[2]int{0, s.top}] = append(groups[[2]int{0, s.top}], s.fields...)
		for _, f := range s.fields {
			key := [2]int{1, int(f.line)}
			groups[key] = append(groups[key], f)
		}
		all = append(all, s.fields...)
	}
	for _, g := range groups {
		if looseCandidate(g, framedScalar) {
			return true
		}
	}
	// SCALAR-WIDE evidence: command words assigned on one line and expanded on another.
	if text, expands := evidenceText(all); expands && strings.Contains(text, "--framework") && namesScan(text) {
		return true
	}
	// A heredoc body is data to `cat` but a program to `bash <<EOF`, so the loose screen
	// reads it as shell too; a body that is not shell at all falls back to its raw text.
	for _, body := range a.heredocs {
		if depth < 4 {
			if _, err := analyseScript(body); err == nil {
				if scanCandidateDepth(body, depth+1) {
					return true
				}
				continue
			}
		}
		if namesScan(body) {
			return true
		}
	}
	return false
}

func looseCandidate(fields []shWord, framedScalar bool) bool {
	words := map[string]bool{}
	for _, f := range fields {
		words[f.value] = true
	}
	// The raw scalar misses a flag spelled across a continuation or with quotes
	// (`--frame\` + newline + `work`); the parsed words do not.
	if (framedScalar || framed(fields)) && words["ksail"] && words["workload"] && words["scan"] {
		return true
	}
	if undecidableScanCandidate(fields) != "" || undecidableShellString(fields) != "" {
		return true
	}
	text, expands := evidenceText(fields)
	return expands && namesScan(text)
}

func namesScan(text string) bool {
	return strings.Contains(text, "ksail") && strings.Contains(text, "workload") && strings.Contains(text, "scan")
}

// evidenceText renders the words that could take part in an invocation — every word that is
// not a fully quoted string — and reports whether any word expands. A fully quoted word is
// an argument, never a command, so prose contributes nothing.
func evidenceText(fields []shWord) (string, bool) {
	var text strings.Builder
	expands := false
	for _, f := range fields {
		if f.expands {
			expands = true
		}
		if f.fullyQuoted {
			continue
		}
		text.WriteString(f.value)
		text.WriteByte(' ')
	}
	return text.String(), expands
}

// collapseQuotedNewlines replaces every newline inside a single- or double-quoted string
// with a space, removing a backslash-newline inside double quotes as bash does. Newlines
// outside quotes — including those ending comments — are kept.
//
// SPIKE: production no longer calls this (the parse tree already knows where every quoted
// string starts and ends); it is kept, parser-backed, because its unit test pins it.
func collapseQuotedNewlines(text string) string {
	f, err := newShellParser().Parse(strings.NewReader(text), "")
	if err != nil {
		return text
	}
	type span struct {
		start, end uint
		folded     string
	}
	var spans []span
	syntax.Walk(f, func(n syntax.Node) bool {
		var folded strings.Builder
		switch q := n.(type) {
		case *syntax.SglQuoted:
			folded.WriteString(strings.ReplaceAll(slice(text, q), "\n", " "))
		case *syntax.DblQuoted:
			if q.Dollar {
				folded.WriteByte('$')
			}
			folded.WriteByte('"')
			for _, p := range q.Parts {
				s := slice(text, p)
				if lit, ok := p.(*syntax.Lit); ok {
					s = lit.Value // the parser has already removed backslash-newline pairs
				}
				folded.WriteString(strings.ReplaceAll(s, "\n", " "))
			}
			folded.WriteByte('"')
		default:
			return true
		}
		spans = append(spans, span{n.Pos().Offset(), n.End().Offset(), folded.String()})
		return false
	})
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out strings.Builder
	last := uint(0)
	for _, s := range spans {
		if s.start < last {
			continue
		}
		out.WriteString(text[last:s.start])
		out.WriteString(s.folded)
		last = s.end
	}
	out.WriteString(text[last:])
	return out.String()
}
