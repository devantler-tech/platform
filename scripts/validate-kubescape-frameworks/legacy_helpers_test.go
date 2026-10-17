package main

import "strings"

// Compatibility seams for the unchanged historical helper unit tests.
// Production admission and extraction use the shared AST analysis.
// Keep the structural reader seam used by existing YAML admission tests.
func runScalars(data []byte) ([]string, error) {
	views, err := workflowScans(data)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(views))
	for _, view := range views {
		out = append(out, view.source)
	}
	return out, nil
}

// collapseQuotedNewlines replaces every newline that falls inside a single- or
// double-quoted string with a space, tracking quote state and backslash escapes the way
// shellFields does, so a quoted string that spans physical lines reads as one word on
// one line. Newlines outside quotes are kept, so line structure survives.
//
// A backslash-newline pair inside double quotes is REMOVED, as bash removes it before it
// builds the quoted argument: `echo "ksail workload \` continued by `scan --framework $X"`
// is one printed string, and leaving the newline in place would split it into two physical
// lines that together spell the scan. Outside quotes the pair is kept, because
// scanCandidate joins unquoted continuations itself on the PHYSICAL line — after deciding
// whether that line is a comment, which a backslash does not continue.
//
// A shell COMMENT — a `#` that begins a word outside quotes — runs to its physical
// newline and opens no quote, whatever it contains. It is copied through untouched
// and its newline is kept, so an unmatched quote inside a comment cannot swallow the
// command on the next line into the comment (the fail-open a quote-state-only fold
// has). A `#` inside a quoted string, or in the middle of a word, is not a comment.
func collapseQuotedNewlines(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	inSingle, inDouble := false, false
	// True when the next byte would begin a shell word: at the start of the text and
	// after unescaped whitespace. Only a `#` in that position opens a comment.
	wordStart := true
	for i := 0; i < len(text); i++ {
		c := text[i]
		atWordStart := wordStart
		wordStart = false
		switch {
		case c == '#' && atWordStart && !inSingle && !inDouble:
			for i < len(text) && text[i] != '\n' {
				out.WriteByte(text[i])
				i++
			}
			// Leave the newline to the next iteration: outside quotes it is kept.
			i--
			wordStart = true
		case c == '\\' && inDouble && i+1 < len(text) && text[i+1] == '\n':
			i++
		case c == '\\' && !inSingle:
			out.WriteByte(c)
			if i+1 < len(text) {
				i++
				out.WriteByte(text[i])
				// An unquoted backslash-newline is removed by bash before the next
				// line is read, so the next byte continues the word the backslash
				// ended: `echo \` then `# x` is `echo # x` (a comment), while
				// `foo\` then `#bar` is `foo#bar` (not one). The pair itself is kept
				// here for scanCandidate's join; only the word-start state carries.
				if text[i] == '\n' && !inDouble {
					wordStart = atWordStart
				}
			}
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			out.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			out.WriteByte(c)
		case c == '\n' && (inSingle || inDouble):
			out.WriteByte(' ')
		default:
			out.WriteByte(c)
			// Whitespace ends a word everywhere; the shell metacharacters end one
			// outside quotes, so `true;# x` and `a|# x` open a comment with no space.
			if c == ' ' || c == '\t' || c == '\n' {
				wordStart = true
			} else if !inSingle && !inDouble && strings.IndexByte(";|&()<>", c) >= 0 {
				wordStart = true
			}
		}
	}
	return out.String()
}

// Shell words that introduce a COMPOUND command — one whose body's execution is not
// decidable from the text. Deliberately limited to reachability constructs: `[[`, `!` and
// `time` change no command's reachability and would refuse ordinary tests for nothing.
//
// `in` is absent because it is never a COMMAND, only a word inside `for`/`case` — and
// those two are here, so the construct is caught at its keyword.
var shellCompoundWords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "select": true,
	"function": true, "coproc": true,
	"{": true, "}": true, "(": true, ")": true,
}

// compoundToken returns the token introducing a compound command when this segment does,
// or "" when it is a plain simple command.
//
// 🔴 ONLY THE FIRST TOKEN, because that is the only place the shell recognises a reserved
// word. Scanning every token instead reads an ARGUMENT as a keyword: `echo done` is a
// simple command, and `ksail workload scan … && echo done` was refused for the `done`
// belonging to `echo`. `shellSplit` has already ended a segment at each operator, so the
// first token of every segment is genuinely in command position.
//
// Whole-token equality, never a substring: the shipped invocation ends in
// `-o "${RUNNER_TEMP}/kubescape.sarif"`, which contains `{` and `}` inside a quoted word.
// `shellSplit` preserves the quote characters, so that word is one token and matches
// nothing here — while a brace GROUP, whose `{` is a token on its own, does.
func compoundToken(segment string) string {
	fields := strings.Fields(segment)
	if len(fields) == 0 {
		return ""
	}
	head := fields[0]
	if shellCompoundWords[head] {
		return head
	}
	// 🔴 A SHAPE TEST, NOT A SUFFIX TEST. A simple command's NAME never contains an
	// unquoted `(`, `)`, `{` or `}`, so any of them here means the segment opens a
	// function definition, a subshell or a group — whatever the spacing.
	//
	// A `strings.HasSuffix(head, "()")` test missed `unused(){`, where bash accepts the
	// brace with no space and the whole thing arrives as ONE field. That form happened to
	// be refused anyway, by the closing `}` landing as its own segment — accidental
	// coverage that a one-line body would not have had, so it is closed at the opener.
	//
	// Quoted spans are skipped, because a first token legitimately carries these
	// characters inside quotes: `"${TOOL}" run` is an ordinary command invocation.
	if unquotedGroupingChar(head) != "" {
		return head
	}
	return ""
}

// unquotedGroupingChar returns the first `(`, `)`, `{` or `}` in tok that is not inside a
// single- or double-quoted span, or "" when there is none.
func unquotedGroupingChar(tok string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c == '\\' && !inSingle:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case inSingle || inDouble:
		case c == '(' || c == ')' || c == '{' || c == '}':
			return string(c)
		}
	}
	return ""
}
