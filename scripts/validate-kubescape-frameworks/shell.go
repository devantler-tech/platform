package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// One parsed view decides admission and extracts the actual framework argument.
// It never executes, expands, or imports shell code.
type shellAnalysis struct {
	source    string
	candidate bool
	scans     []string
	err       error
	effects   shellEffects
}

type shellEffects struct {
	errexit      *bool
	successTraps map[string]bool
	successExit  bool
}

func analyzeShell(source string) shellAnalysis { return analyzeShellDepth(source, 0) }

func analyzeShellDepth(source string, depth int) shellAnalysis {
	result := shellAnalysis{source: source}
	if depth > 8 {
		result.candidate = true
		result.err = fmt.Errorf("nested executable shell input is not decidable from the text")
		return result
	}
	// Actions replaces these before bash starts. Preserve their dynamic-word
	// character without asking the shell parser to interpret Actions syntax.
	input, maskErr := maskActionsShellExpressions(source)
	if maskErr != nil {
		result.candidate = true
		result.err = maskErr
		return result
	}
	tree, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(input), "")
	if err != nil {
		result.candidate = strings.Contains(source, "ksail") || strings.Contains(source, "--framework")
		result.err = fmt.Errorf("shell program is not decidable from the text: %w; quote the text if it is prose", err)
		return result
	}
	var stack []syntax.Node
	errexit, successExit := true, false
	successTraps := make(map[string]bool)
	functions := make(map[string]shellEffects)
	applyEffects := func(effects shellEffects) {
		if effects.errexit != nil {
			errexit, result.effects.errexit = *effects.errexit, effects.errexit
		}
		for signal, masks := range effects.successTraps {
			successTraps[signal] = masks
			if result.effects.successTraps == nil {
				result.effects.successTraps = make(map[string]bool)
			}
			result.effects.successTraps[signal] = masks
		}
		if effects.successExit {
			successExit, result.effects.successExit = true, true
		}
	}
	refuse := func(reason string) {
		result.candidate = true
		if result.err == nil {
			result.err = fmt.Errorf("scan is not decidable from the text: %s; quote the text if it is prose", reason)
		}
	}
	payloadMayScan := func(text string) bool {
		view := analyzeShellDepth(text, depth+1)
		if view.candidate || view.err != nil {
			return true
		}
		// A bounded command string spelling just one scan command word is
		// already an ambiguous execution input under the existing contract.
		if literalShellWords.MatchString(text) {
			for _, word := range strings.Fields(text) {
				switch filepath.Base(word) {
				case "ksail", "workload", "scan":
					return true
				}
			}
		}
		return false
	}
	syntax.Walk(tree, func(node syntax.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, node)
		switch n := node.(type) {
		case *syntax.FuncDecl:
			if n.Name.Value == "ksail" {
				refuse("a local function replaces the scanner executable")
			}
			view := analyzeShellDepth(printShellNode(n.Body), depth+1)
			if view.err != nil {
				refuse("local function execution cannot be certified from the bounded text")
			}
			functions[n.Name.Value] = view.effects
		case *syntax.CallExpr:
			if len(n.Args) == 0 {
				return true
			}
			raw := make([]string, len(n.Args))
			words := make([]string, len(n.Args))
			static := make([]bool, len(n.Args))
			envAssignments := make([]bool, len(n.Args))
			for i, word := range n.Args {
				raw[i] = printShellNode(word)
				words[i], static[i] = literalShellWord(word)
				envAssignments[i] = shellEnvAssignment(word)
			}
			command := words[0]
			stmt := enclosingShellStmt(stack)
			if !static[0] {
				refuse("command position is dynamically assembled")
			}
			executable, resolutionErr := resolveShellExecutable(words, static, envAssignments)
			if resolutionErr != "" {
				refuse(resolutionErr)
			}
			effectiveWords, effectiveStatic := words[executable:], static[executable:]
			effectiveCommand := filepath.Base(effectiveWords[0])
			deferred := false
			for _, parent := range stack {
				if _, ok := parent.(*syntax.FuncDecl); ok {
					deferred = true
				}
			}
			if !deferred {
				if effects, found := functions[effectiveCommand]; found {
					applyEffects(effects)
				}
				if effectiveCommand == "set" {
					if mode := shellErrexitChange(effectiveWords, effectiveStatic); mode != nil {
						applyEffects(shellEffects{errexit: mode})
					}
				}
				if effectiveCommand == "exit" && (len(effectiveWords) == 1 || !effectiveStatic[1] || effectiveWords[1] == "0") {
					applyEffects(shellEffects{successExit: true})
				}
			}

			// Text reparsed by another interpreter is an execution boundary,
			// including its stdin. Ordinary data consumers never reparse it.
			if effectiveCommand == "eval" {
				literal := true
				for i := 1; i < len(effectiveWords); i++ {
					if !effectiveStatic[i] {
						refuse("eval assembles executable code from dynamic operands")
						literal = false
					}
				}
				if literal {
					text := strings.Join(effectiveWords[1:], " ")
					if !deferred {
						applyEffects(analyzeShellDepth(text, depth+1).effects)
					}
					if payloadMayScan(text) {
						refuse("eval reparses executable scan text")
					}
				}
			}
			if effectiveCommand == "trap" && len(effectiveWords) > 2 && effectiveStatic[1] && effectiveWords[1] != "-p" && !deferred {
				for _, signal := range effectiveWords[2:] {
					if signal == "EXIT" || signal == "0" || signal == "ERR" {
						if signal == "0" {
							signal = "EXIT"
						}
						mask := effectiveWords[1] != "-" && analyzeShellDepth(effectiveWords[1], depth+1).effects.successExit
						applyEffects(shellEffects{successTraps: map[string]bool{signal: mask}})
					}
				}
			}
			if effectiveCommand == "trap" || effectiveCommand == "alias" {
				for i := 1; i < len(effectiveWords); i++ {
					text := effectiveWords[i]
					if effectiveCommand == "alias" {
						name, value, found := strings.Cut(text, "=")
						if !found {
							continue
						}
						if name == "ksail" {
							refuse("a local alias replaces the scanner executable")
						}
						text = value
					}
					if effectiveStatic[i] && payloadMayScan(text) {
						refuse(effectiveCommand + " reparses scan text")
					}
					if !effectiveStatic[i] {
						refuse(effectiveCommand + " assembles executable text")
					}
				}
			}
			if effectiveCommand == "xargs" {
				index, reason := xargsExecutable(effectiveWords, effectiveStatic)
				if reason != "" {
					refuse(reason)
				} else if index >= 0 {
					if executionWrapper(filepath.Base(effectiveWords[index])) {
						refuse("xargs can supply a delegated executable from stdin")
					}
					nested, nestedErr := resolveShellExecutable(effectiveWords[index:], effectiveStatic[index:], envAssignments[executable+index:])
					if nestedErr != "" {
						refuse("xargs delegates an undecidable executable: " + nestedErr)
					}
					index += nested
					if filepath.Base(effectiveWords[index]) == "ksail" {
						refuse("xargs can append scan command words from stdin")
					} else if shellInterpreter(filepath.Base(effectiveWords[index])) || effectiveWords[index] == "eval" {
						refuse("xargs constructs input for a shell interpreter")
					}
				}
			}
			if shellInterpreter(effectiveCommand) && stmt != nil {
				codeIndex, readsStdin, modeErr := shellInvocationMode(effectiveWords, effectiveStatic)
				if modeErr != "" {
					refuse(modeErr)
				}
				if codeIndex >= 0 {
					if !effectiveStatic[codeIndex] {
						refuse("shell interpreter assembles executable code from a dynamic operand")
					} else if payloadMayScan(effectiveWords[codeIndex]) {
						refuse("shell interpreter reparses executable scan text")
					}
				}
				redirect := effectiveShellStdin(stack)
				for _, parent := range stack {
					if pipeline, ok := parent.(*syntax.BinaryCmd); ok && (pipeline.Op.String() == "|" || pipeline.Op.String() == "|&") && nodeContains(pipeline.Y, n) && readsStdin && redirect == nil {
						refuse("shell interpreter reparses pipeline input")
					}
				}
				if redirect != nil && readsStdin {
					switch redirect.Op.String() {
					case "<&", ">&", ">", ">>", ">|", "<>":
						refuse("shell interpreter stdin file-descriptor source cannot be read")
					case "<<", "<<-", "<<<", "<":
						text := printShellNode(redirect.Word)
						if redirect.Hdoc != nil {
							text = printShellNode(redirect.Hdoc)
						}
						if value, ok := literalShellWord(redirect.Word); ok && redirect.Hdoc == nil {
							text = value
						}
						if payloadMayScan(text) || (strings.Contains(text, "ksail") && strings.Contains(text, "workload") && strings.Contains(text, "scan")) {
							refuse("shell interpreter reparses its redirected input")
						}
					}
				}
			}

			scanArgs, isScan, flagsErr := resolvedScanArguments(words, static)
			if !isScan {
				if reason := undecidableScanCandidate(raw); reason != "" {
					refuse(reason)
				}
				if allExpandedBeforeFramework(raw) {
					refuse("all command words before --framework are dynamically assembled")
				}
				return true
			}
			result.candidate = true
			if !deferred && (!errexit || successTraps["EXIT"] || successTraps["ERR"] || successExit) {
				refuse("scanner failure handling is disabled or a successful exit can replace the gate")
				return true
			}
			if flagsErr != "" {
				refuse(flagsErr)
				return true
			}
			if command != "ksail" || len(n.Assigns) != 0 {
				refuse("scan command has a path, wrapper, assignment or dynamic command prefix")
				return true
			}
			if reason := undecidableOptionWord(raw[scanArgs:]); reason != "" {
				refuse(reason)
				return true
			}
			if shellStatusUnsafe(stack, input) {
				refuse("scan is conditional, nested or its failure is discarded")
				return true
			}
			argument, found := "", false
			for i := scanArgs; i < len(raw); i++ {
				if raw[i] == "--framework" && i+1 < len(raw) {
					argument, found = raw[i+1], true
					break
				}
				if value, ok := strings.CutPrefix(raw[i], "--framework="); ok {
					argument, _ = resolveToken(value)
					found = true
					break
				}
			}
			if !found {
				refuse("scan has no explicit readable framework list")
				return true
			}
			result.scans = append(result.scans, argument)
		}
		return true
	})
	return result
}

// set changes the current shell; -- separates positional data from options.
// A dynamic leading option cannot prove that errexit remains enabled.
func shellErrexitChange(words []string, static []bool) *bool {
	var mode *bool
	for i := 1; i < len(words); i++ {
		if !static[i] {
			unknown := false
			return &unknown
		}
		word := words[i]
		if word == "--" || (!strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "+")) {
			break
		}
		if word == "-o" || word == "+o" {
			if i+1 == len(words) {
				continue
			}
			i++
			if !static[i] {
				unknown := false
				return &unknown
			}
			if words[i] != "errexit" {
				continue
			}
		} else if !strings.Contains(word[1:], "e") {
			continue
		}
		enabled := strings.HasPrefix(word, "-")
		mode = &enabled
	}
	return mode
}

// The same bounded executable resolution applies to direct and delegated calls.
// env split-string has a different language from Bash; it is refused explicitly.
func resolveShellExecutable(words []string, static, envAssignments []bool) (int, string) {
	current := 0
	for current < len(words) {
		if !static[current] {
			return current, "wrapper command position is dynamically assembled"
		}
		index, reason := wrappedExecutable(words[current:], static[current:], envAssignments[current:])
		if reason != "" {
			return current, reason
		}
		if index <= 0 {
			return current, ""
		}
		current += index
	}
	return current, ""
}

// Redirections on enclosing groups feed descendants, while closer statements
// and later fd0 redirections override them. Other file descriptors are data.
func effectiveShellStdin(stack []syntax.Node) *syntax.Redirect {
	var input *syntax.Redirect
	for _, node := range stack {
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			continue
		}
		for _, redirect := range stmt.Redirs {
			if redirect.N != nil && redirect.N.Value != "0" {
				continue
			}
			switch redirect.Op.String() {
			case "<<", "<<-", "<<<", "<", "<&":
				input = redirect
			case ">&", ">", ">>", ">|", "<>":
				if redirect.N != nil && redirect.N.Value == "0" {
					input = redirect
				}
			}
		}
	}
	return input
}

// xargs executes a separate command. Read only options with known operand arity;
// dynamic or unsupported executable resolution cannot be certified scan-free.
func xargsExecutable(words []string, static []bool) (int, string) {
	for i := 1; i < len(words); i++ {
		if !static[i] {
			return -1, "xargs command position is dynamically assembled"
		}
		word := words[i]
		switch word {
		case "-0", "-r", "-t", "-p", "-x", "--null", "--no-run-if-empty", "--verbose", "--interactive", "--exit":
			continue
		case "-I", "-n", "-L", "-P", "-s", "-a", "-E":
			i++
			if i >= len(words) || !static[i] {
				return -1, "xargs option operand is not decidable"
			}
			continue
		case "--":
			if i+1 < len(words) && static[i+1] {
				return i + 1, ""
			}
			return -1, "xargs command position is not decidable"
		}
		if strings.HasPrefix(word, "-") {
			return -1, "xargs option resolution is not decidable"
		}
		return i, ""
	}
	return -1, ""
}

// Read the expression envelope, including quoted }} and doubled quote escapes.
// Keep newlines so an Actions value cannot change parser line boundaries.
func maskActionsShellExpressions(source string) (string, error) {
	if len(source) > 1<<20 {
		return "", fmt.Errorf("shell input exceeds the bounded parser size")
	}
	var out strings.Builder
	for i := 0; i < len(source); {
		if !strings.HasPrefix(source[i:], "${{") {
			out.WriteByte(source[i])
			i++
			continue
		}
		j, quote, newlines := i+3, byte(0), 0
		for ; j < len(source); j++ {
			c := source[j]
			if c == '\n' {
				newlines++
			}
			if quote != 0 {
				if c == quote {
					if j+1 < len(source) && source[j+1] == quote {
						j++
						continue
					}
					quote = 0
				}
				continue
			}
			if c == '\'' || c == '"' {
				quote = c
				continue
			}
			if strings.HasPrefix(source[j:], "}}") {
				break
			}
		}
		if j >= len(source) {
			return "", fmt.Errorf("Actions expression envelope is not decidable from the text")
		}
		if strings.TrimSpace(source[i+3:j]) == "" {
			return "", fmt.Errorf("empty Actions expression cannot identify an executable value")
		}
		out.WriteString("${__ACTIONS_EXPRESSION__}")
		out.WriteString(strings.Repeat("\n", newlines))
		i = j + 2
	}
	return out.String(), nil
}

// Known wrappers can put the executable after options and env assignments.
// Ordinary dynamic env VALUES do not become executable positions.
func wrappedExecutable(words []string, static, envAssignments []bool) (int, string) {
	switch filepath.Base(words[0]) {
	case "env":
		for i := 1; i < len(words); i++ {
			if envAssignments[i] {
				continue
			}
			if !static[i] {
				return i, ""
			}
			word := words[i]
			if word == "--unset" || word == "--chdir" || word == "--argv0" {
				i++
				continue
			}
			if word == "--" {
				if i+1 < len(words) {
					return i + 1, ""
				}
				return -1, ""
			}
			if word == "--split-string" || strings.HasPrefix(word, "--split-string=") {
				return -1, "env split-string reparses executable text outside the supported Bash grammar"
			}
			if strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--") {
				for offset := 1; offset < len(word); offset++ {
					switch word[offset] {
					case 'S':
						return -1, "env split-string reparses executable text outside the supported Bash grammar"
					case 'u', 'C', 'a':
						if offset+1 == len(word) {
							i++
						}
						offset = len(word)
					case 'i', '0', 'v', '-':
					default:
						return -1, "env option operand arity is not decidable"
					}
				}
				continue
			}
			if strings.HasPrefix(word, "-") {
				continue
			}
			if strings.Contains(word, "=") {
				continue
			}
			return i, ""
		}
	case "command", "exec", "sudo", "builtin":
		for i := 1; i < len(words); i++ {
			if filepath.Base(words[0]) == "command" && static[i] && strings.HasPrefix(words[i], "-") && !strings.HasPrefix(words[i], "--") && strings.Trim(words[i][1:], "pvV") == "" && strings.ContainsAny(words[i], "vV") {
				return -1, ""
			}
			if !static[i] || !strings.HasPrefix(words[i], "-") {
				return i, ""
			}
			if (words[0] == "exec" && words[i] == "-a") || (filepath.Base(words[0]) == "sudo" && (words[i] == "-u" || words[i] == "-g" || words[i] == "-h" || words[i] == "-p" || words[i] == "-C" || words[i] == "-T" || words[i] == "-R" || words[i] == "-D")) {
				i++
			}
		}
	}
	return -1, ""
}

func executionWrapper(base string) bool {
	switch base {
	case "env", "command", "exec", "sudo", "builtin":
		return true
	}
	return false
}

func literalEnvAssignment(raw string) bool {
	name, _, found := strings.Cut(raw, "=")
	if !found || name == "" {
		return false
	}
	for i, c := range name {
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// A quoted expansion in an env assignment changes only its value. Unquoted
// expansions can split into additional argv words and cannot certify that.
func shellEnvAssignment(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	multiword := false
	syntax.Walk(word, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			return false
		}
		if parameter, ok := node.(*syntax.ParamExp); ok && !parameter.Length {
			if parameter.Param.Value == "@" || printShellNode(parameter.Index) == "@" || parameter.Excl {
				multiword = true
			}
		}
		return true
	})
	if multiword {
		return false
	}
	var prefix strings.Builder
	complete := true
	var visit func([]syntax.WordPart, bool) bool
	visit = func(parts []syntax.WordPart, quoted bool) bool {
		for _, part := range parts {
			if double, ok := part.(*syntax.DblQuoted); ok {
				if double.Dollar || !visit(double.Parts, true) {
					return false
				}
				continue
			}
			value, literal := literalShellParts([]syntax.WordPart{part}, quoted)
			if !literal {
				if !quoted {
					return false
				}
				complete = false
				continue
			}
			if complete {
				prefix.WriteString(value)
			}
		}
		return true
	}
	return visit(word.Parts, false) && literalEnvAssignment(prefix.String())
}

// Resolve the interpreter's execution mode once. A named script receives stdin
// as data; -c reparses its operand; -s or an absent script reparses stdin.
func shellInvocationMode(words []string, static []bool) (int, bool, string) {
	stdin := false
	for i := 1; i < len(words); i++ {
		if !static[i] {
			return -1, false, "shell interpreter option or script input is dynamically assembled"
		}
		word := words[i]
		if word == "--" {
			if stdin || i+1 == len(words) {
				return -1, true, ""
			}
			if !static[i+1] {
				return -1, false, "shell script operand is dynamically assembled"
			}
			return -1, words[i+1] == "-", ""
		}
		if word == "-" || (!strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "+")) {
			return -1, stdin || word == "-", ""
		}
		if word == "-o" || word == "+o" || word == "-O" || word == "+O" || word == "--init-file" || word == "--rcfile" {
			i++
			if i >= len(words) || !static[i] {
				return -1, false, "shell option operand is not decidable"
			}
			continue
		}
		if !strings.HasPrefix(word, "--") && strings.Contains(word, "c") {
			if i+1 < len(words) {
				return i + 1, false, ""
			}
			return -1, false, "shell code operand is missing"
		}
		if !strings.HasPrefix(word, "--") && strings.Contains(word, "s") {
			stdin = strings.HasPrefix(word, "-")
		}
	}
	return -1, true, ""
}

func printShellNode(node syntax.Node) string {
	if node == nil {
		return ""
	}
	var buffer bytes.Buffer
	if err := syntax.NewPrinter(syntax.Minify(true)).Print(&buffer, node); err != nil {
		return ""
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

func literalShellWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	if strings.Contains(printShellNode(word), "${__ACTIONS_EXPRESSION__}") {
		return "", false
	}
	return literalShellParts(word.Parts, false)
}

func literalShellParts(parts []syntax.WordPart, double bool) (string, bool) {
	var value strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			for i := 0; i < len(p.Value); i++ {
				c := p.Value[i]
				if c == '\\' && i+1 < len(p.Value) {
					next := p.Value[i+1]
					if next == '\n' {
						i++
						continue
					}
					if !double || strings.ContainsRune("$`\\\"", rune(next)) {
						i++
						value.WriteByte(next)
						continue
					}
				}
				if !double && (strings.ContainsRune("*?{", rune(c)) || (c == '[' && strings.Contains(p.Value[i+1:], "]"))) {
					return "", false
				}
				value.WriteByte(c)
			}
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			value.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			nested, ok := literalShellParts(p.Parts, true)
			if !ok {
				return "", false
			}
			value.WriteString(nested)
		default:
			return "", false
		}
	}
	return value.String(), true
}

func shellInterpreter(base string) bool {
	switch base {
	case "bash", "sh", "dash", "zsh", "ksh":
		return true
	}
	return false
}

func enclosingShellStmt(stack []syntax.Node) *syntax.Stmt {
	for i := len(stack) - 1; i >= 0; i-- {
		if stmt, ok := stack[i].(*syntax.Stmt); ok {
			return stmt
		}
	}
	return nil
}

// Cobra permits root flags on either side of a subcommand. Only flags with
// known operand arity can be skipped; uncertain resolution refuses a candidate.
func resolvedScanArguments(words []string, static []bool) (int, bool, string) {
	for start, word := range words {
		if filepath.Base(word) != "ksail" || !static[start] {
			continue
		}
		stage := 0
		for i := start + 1; i < len(words); i++ {
			if !static[i] {
				break
			}
			switch words[i] {
			case "--benchmark", "--experimental", "--verbose", "-v":
				continue
			case "--config":
				if i+1 >= len(words) || !static[i+1] {
					return 0, true, "root flag operand cannot be read"
				}
				i++
				continue
			}
			if strings.HasPrefix(words[i], "--config=") {
				continue
			}
			flag, value, hasValue := strings.Cut(words[i], "=")
			if hasValue && (flag == "--benchmark" || flag == "--experimental" || flag == "--verbose" || flag == "-v") {
				if _, err := strconv.ParseBool(value); err != nil {
					return 0, true, "root boolean flag operand cannot be read"
				}
				continue
			}
			if stage == 0 && words[i] == "workload" {
				stage++
				continue
			}
			if stage == 1 && words[i] == "scan" {
				if start != 0 {
					return i + 1, true, "scan executes behind a command wrapper"
				}
				return i + 1, true, ""
			}
			break
		}
	}
	return 0, false, ""
}

func nodeContains(container syntax.Node, target syntax.Node) bool {
	return container != nil && container.Pos().Offset() <= target.Pos().Offset() && container.End().Offset() >= target.End().Offset()
}

func shellStatusUnsafe(stack []syntax.Node, source string) bool {
	call := stack[len(stack)-1]
	for _, parent := range stack {
		switch n := parent.(type) {
		case *syntax.Stmt:
			if n.Negated || n.Background || n.Coprocess {
				return true
			}
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause, *syntax.FuncDecl, *syntax.Block, *syntax.Subshell:
			return true
		case *syntax.BinaryCmd:
			switch n.Op.String() {
			case "|", "|&":
				return true
			case "&&":
				if nodeContains(n.Y, call) {
					return true
				}
			case "||":
				if nodeContains(n.Y, call) {
					return true
				}
				if nodeContains(n.X, call) {
					start, end := n.Y.Pos().Offset(), n.Y.End().Offset()
					if int(end) > len(source) || !provablyFails(source[start:end]) {
						return true
					}
				}
			}
		}
	}
	return false
}
