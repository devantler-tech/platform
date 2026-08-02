// Command validate-publication-order checks that production is never pointed at
// an artifact whose supply-chain evidence is still being assembled.
//
// The prod deploy publishes the platform manifests to a mutable tag, signs them
// with keyless cosign, attests an SBOM and SLSA provenance, and only then tells
// Flux to reconcile. That order is the whole guarantee: reordering any of it
// leaves a window in which the cluster's root source — which delivers every
// controller, tenant binding and policy — resolves bytes that carry no
// signature or no attestation.
//
// Nothing about that window is visible in a green deploy. The steps all
// succeed, the cluster converges, and the artifact ends up fully signed a
// minute later; the defect is only that Flux could have looked in between. So
// the ordering has no natural regression signal, and a later edit that moves
// the reconcile trigger up to "fail faster" would be indistinguishable from an
// improvement. This turns that into a pull-request failure.
//
// Scope: this pins the order of the steps that already exist. It does NOT
// establish that publication is atomic — the mutable tag is still moved by the
// push step before signing begins, which is the substance of
// devantler-tech/platform#2627 and needs a staging reference to fix. Do not
// read a green run here as that issue being closed.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// step is one entry of runs.steps, reduced to what identifies it here.
type step struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
	// Shell decides whether a failing command fails the step at all. GitHub runs `shell: bash` with
	// `-e`; any other shell is not guaranteed to, so a required operation there cannot be assumed to
	// gate the step. Composite actions require this field on every run step.
	Shell string `yaml:"shell"`
	// If is read because ordering is positional: a step keeps its position while carrying a
	// condition that never fires, so the sequence can be satisfied by steps that do not run.
	If string `yaml:"if"`
	// With carries an action's inputs, which is where an attestation says WHAT it attests. Position
	// and action reference together still leave the subject free, and an attestation of the wrong
	// subject is evidence that looks complete and covers nothing this run published.
	With map[string]string `yaml:"with"`
	// Env is read because two of its names change the shell BEFORE the run block's first line, so a
	// step can shadow a required command without the script containing anything unusual.
	Env map[string]string `yaml:"env"`
}

type action struct {
	Runs struct {
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
}

// marker locates one step of the publication sequence. Matching on what a step
// DOES — the command it runs or the action it uses — rather than on its name
// keeps the check working when a name is reworded, and failing when the step it
// describes actually disappears.
type marker struct {
	// label is how the step is described in an error message.
	label string
	// match reports whether a step is this one.
	match func(step) bool
	// hint replaces the generic "why did my step stop matching" guidance when this marker needs
	// different advice. The default text is about shell commands, which is useless for a step that
	// runs no shell: an attestation stops matching because of its INPUTS, and being told to check
	// for `set +e` sends the reader looking in the wrong place entirely.
	hint string
}

// executedCommand is one command the shell will actually run, reduced to what the markers ask
// about: its words exactly as written.
//
// Deciding "actually run" from the shell GRAMMAR rather than from the text is what collapses a whole
// family of bypasses into one rule. A comment is not a word; a here-document body is an argument to
// the command that opened it, whatever quoting the delimiter uses; and `||`, `&&`, pipelines,
// subshells, function bodies, backgrounding, negation and every conditional construct produce a
// statement that is not a plain call. None of them can therefore present a required operation as an
// executed command.
//
// Earlier revisions enumerated those shapes one at a time — a comment, then a step condition, then a
// block, then a one-line `if`, then `&&`, then `echo`, then a here-document — and each omission was a
// silent bypass rather than a failure. That is a blocklist, and the list never ended. Asking the
// parser what runs is the allowlist equivalent.
type executedCommand struct {
	// words is argv as written in the source, quoting included, so an argument can be compared
	// against the exact form the contract requires (`"${REF}"`) rather than against its expansion.
	words []string
	// lits is argv after QUOTE REMOVAL, for the questions that are about what bash will do rather
	// than about how the contract is spelled. The two differ, and reading the source form for a
	// shell decision is a bypass: `set '+e'` disables errexit, because bash strips the quotes before
	// interpreting the word, while the source form `'+e'` matches no flag pattern at all.
	//
	// A word whose value is not statically known — it expands a parameter or runs a substitution —
	// has no entry here, which is what litOK records. Callers must fail closed on that rather than
	// silently comparing against an empty string.
	lits  []string
	litOK []bool
}

// literalOf returns a word's value after quote removal, and whether that value is static.
//
// Single quotes, double quotes and backslash escapes all disappear before bash interprets a word, so
// `+e`, `'+e'`, `"+e"` and `\+e` are the same argument. Anything whose value depends on the
// environment — a parameter expansion, a command substitution, arithmetic — is NOT static, and is
// reported as such so a caller can refuse to guess.
func literalOf(w *syntax.Word) (string, bool) {
	var sb strings.Builder

	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.Lit:
			// A backslash escape survives into Lit.Value; bash removes it.
			sb.WriteString(strings.ReplaceAll(p.Value, "\\", ""))
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, isLit := inner.(*syntax.Lit)
				if !isLit {
					return "", false
				}

				sb.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}

	return sb.String(), true
}

// assignment is one variable assignment, kept with enough provenance to tell a resolved value from a
// written-down one.
type assignment struct {
	// text is the value's source, so an exact required form can be compared.
	text string
	// consumesPublishedTag reports whether the value was RESOLVED by running something that reads
	// the tag this run just published. A digest that was typed is not a digest that was resolved,
	// and only the second binds a signature to this run.
	consumesPublishedTag bool
}

// parseExecuted returns the commands a run block executes at top level.
//
// A parse failure yields none, so an unparseable script can never satisfy a required marker: the
// guard fails closed rather than falling back to matching text.
func parseExecuted(run string, errexit bool) []executedCommand {
	file, err := syntax.NewParser().Parse(strings.NewReader(run), "")
	if err != nil {
		return nil
	}

	out := make([]executedCommand, 0, len(file.Stmts))
	shadowed := declaredNames(file)

	for _, stmt := range file.Stmts {
		call, isCall := stmt.Cmd.(*syntax.CallExpr)
		if !isCall {
			continue
		}

		// `! cmd` inverts the exit status and `cmd &` detaches it. Neither lets the step fail when
		// the operation does, which is the only reason requiring the operation means anything.
		if stmt.Negated || stmt.Background {
			continue
		}

		cmd := newExecutedCommand(run, call)

		// An unconditional `exit` or `exec` at the top level ends this shell, so nothing written
		// after it runs at all. Continuing to scan credited a later `cosign sign` as executed even
		// though control never reached it: the step still exits zero, the digest output is already
		// exported, and both attestations plus reconciliation advance over an artifact that was
		// never signed. Stop here rather than skipping the statement — every subsequent command is
		// equally unreachable.
		//
		// Only the unconditional top-level form terminates. A negated or backgrounded one was
		// already skipped above, and one nested inside an if/loop/function is not reached by this
		// walk, which iterates top-level statements only.
		if terminatesShell(cmd) {
			break
		}

		// `set` changes how the shell treats a later failure, so it has to be tracked as state
		// rather than matched as a shape. Applied before the errexit test so `set -e` enables
		// itself, which is what makes an explicit re-enable after `set +e` work.
		if toggled, ok := errexitToggle(cmd); ok {
			errexit = toggled

			continue
		}

		// Grammar says this command runs; errexit says its failure reaches the step status. A
		// required operation needs both — under `set +e` cosign can fail, the next command succeed,
		// and the step still exit zero.
		if !errexit {
			continue
		}

		// ...and name resolution says the words actually reach the program they name. Bash looks up
		// aliases, then functions, before PATH, so either `cosign() { true; }` or
		// `alias cosign=true` turns the required invocation into a no-op that is otherwise
		// indistinguishable from the real thing.
		//
		// A command whose executable is not statically known (`"${SIGNER}" sign …`) cannot be proved
		// to reach the real program either, so it does not count as one.
		exec, known := cmd.name()
		if !known || shadowed[exec] {
			continue
		}

		out = append(out, cmd)
	}

	return out
}

// declaredNames returns every name the script rebinds ahead of PATH — as a function or as an alias.
//
// Bash resolves an alias first, a function next, and only then searches PATH, so both rebindings
// have the same consequence for this guard: the words of a required operation stop reaching the
// program they name. `alias cosign=true` combined with `shopt -s expand_aliases` (which a run block
// may set, and which the parser does not apply) expands a textually perfect, failure-propagating,
// top-level `cosign sign … "${REF}"` to `true`, producing no signature.
//
// Aliases expand at READ time rather than at execution time, so an alias defined and used inside the
// same compound command does not take effect — but that distinction is not worth encoding. Treating
// any rebinding of a required name as disqualifying is the conservative direction and matches how
// functions are already handled: the remedy, given in the error message, is to name the wrapper
// something else.
// unmodeledShellState reports a construct that changes how the shell RESOLVES a command or
// PROPAGATES its failure and that this guard does not interpret, naming it for the error message.
//
// This is the state axis of the rule executedCommand already applies to the grammar axis. That one
// asks the parser what RUNS instead of enumerating the ways a line can fail to run; this one bounds
// what can change the MEANING of what runs. The guard models a fixed set — errexit, aliases, shell
// functions, namerefs — and previously skipped every other construct, so each unmodeled one was a
// silent bypass rather than a failure: `hash -p /usr/bin/true cosign` rebinds the lookup,
// `trap 'exit 0' ERR` swallows the failure, `set -n` stops execution entirely, and `source`/`eval`
// run text this file never sees. Each was reported separately, and the list did not end.
//
// So the direction is inverted: a construct is allowed here only because it has been reasoned about,
// and anything else fails closed with its name in the message. The remedy is always the same —
// express the step without it, or extend the model deliberately.
//
// The walk DESCENDS into nested constructs, unlike the top-level statement iteration that decides
// which commands run. The two questions differ: a command nested in a conditional may never execute,
// but a block, loop or branch body runs in the CURRENT shell, so `{ trap 'exit 0' ERR; }` changes
// exactly as much state as the bare form does. Depth is what this walk must not stop at.
func unmodeledShellState(file *syntax.File) (string, bool) {
	found := ""
	// The state axis is only sound where BOTH halves agree. This walk sees every node, but
	// parseExecuted tracks errexit from top-level statements alone, so a `set` anywhere else
	// changes the shell without moving the tracked state.
	tracked := stateTrackedCalls(file)
	subshell := callsInSubshells(file)

	syntax.Walk(file, func(node syntax.Node) bool {
		if found != "" {
			return false
		}

		if assign, isAssign := node.(*syntax.Assign); isAssign && assign.Name != nil {
			// PATH decides where every unqualified name resolves, and BASH_ENV/ENV name a file the
			// shell reads before the script. None of the three is interpreted here.
			switch assign.Name.Value {
			case "PATH", "BASH_ENV", "ENV":
				found = "an assignment to " + assign.Name.Value

				return false
			}
		}

		call, isCall := node.(*syntax.CallExpr)
		if !isCall || len(call.Args) == 0 {
			return true
		}

		// The Assign check above only sees `PATH=…`. A builtin can write the same variable while
		// naming it as an ARGUMENT — `printf -v PATH …`, `read PATH` — which produces no Assign
		// node at all, so the walk waved it through and a shadowed `cosign` was credited as the
		// real signature. Same destinations, same consequence, different spelling; reuse the
		// existing detector rather than a second, differently-wrong one.
		for _, protected := range [...]string{"PATH", "BASH_ENV", "ENV"} {
			if builtinWritesTo(call, protected) {
				found = "a builtin write to " + protected

				return false
			}
		}

		lits, litOK := wordsOf(call.Args)

		start, executes := commandStart(lits, litOK)
		if !executes || start >= len(lits) || !litOK[start] {
			return true
		}

		switch name := lits[start]; name {
		case "hash", "trap", "shopt", "enable", "unalias", "eval", "source", ".":
			found = "`" + name + "`"

			return false
		case "bash", "sh", "dash", "ksh", "zsh":
			// An interpreter given inline code runs text this file never parses, exactly as `eval`
			// does. One naming a SCRIPT is ordinary and is resolved through by scriptTarget.
			for _, arg := range lits[start+1:] {
				if strings.HasPrefix(arg, "-") && strings.Contains(arg, "c") {
					found = "`" + name + " " + arg + "`"

					return false
				}
			}
		case "set":
			// A brace group, branch, loop or function body is not a top-level statement, so
			// errexitToggle never sees this call — while bash still applies it, because all of
			// them run in the CURRENT shell. Accepting them credited a later `cosign` as
			// failure-propagating when errexit was in fact off.
			//
			// A subshell is the opposite case and must still pass: `$( set -euo pipefail; … )`
			// changes only the child, so the parent state this file tracks is correctly
			// unchanged. Rejecting it would fail ordinary scripting.
			if !tracked[call] && !subshell[call] {
				found = "`set` in a compound construct that runs in the step's own shell"

				return false
			}

			if opt, ok := unmodeledSetOption(lits[start+1:], litOK[start+1:]); ok {
				if opt == nonLiteralOption {
					found = "`set` with an option whose value is not statically known"
				} else {
					found = "`set " + opt + "`"
				}

				return false
			}
		}

		return true
	})

	return found, found != ""
}

// stateTrackedCalls returns the calls whose shell-state effect parseExecuted actually evaluates:
// the top-level statements it iterates, minus the negated and backgrounded ones it skips. Anything
// outside this set can change the shell without the tracked state moving, so it fails closed.
func stateTrackedCalls(file *syntax.File) map[*syntax.CallExpr]bool {
	out := make(map[*syntax.CallExpr]bool, len(file.Stmts))

	for _, stmt := range file.Stmts {
		if stmt.Negated || stmt.Background {
			continue
		}

		if call, isCall := stmt.Cmd.(*syntax.CallExpr); isCall {
			out[call] = true
		}
	}

	return out
}

// callsInSubshells returns the calls that run in a CHILD shell rather than the step's own.
//
// This is the line between a `set` that must fail closed and one that must pass: a brace group,
// branch or loop applies to the shell parseExecuted is tracking, while a command substitution,
// explicit subshell, process substitution, backgrounded statement or pipeline stage applies only
// to a child that exits. `$( set -euo pipefail; crane digest … )` is ordinary scripting and stays
// legal for exactly that reason.
func callsInSubshells(file *syntax.File) map[*syntax.CallExpr]bool {
	out := make(map[*syntax.CallExpr]bool)

	markAll := func(n syntax.Node) {
		syntax.Walk(n, func(inner syntax.Node) bool {
			if call, isCall := inner.(*syntax.CallExpr); isCall {
				out[call] = true
			}

			return true
		})
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		switch typed := node.(type) {
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
			markAll(node)
		case *syntax.Stmt:
			if typed.Background {
				markAll(typed)
			}
		case *syntax.BinaryCmd:
			// Each stage of a pipeline runs in its own shell, so neither side can move the
			// state this file tracks. `&&`/`||` do NOT create one and are deliberately absent.
			if typed.Op == syntax.Pipe || typed.Op == syntax.PipeAll {
				markAll(typed.X)
				markAll(typed.Y)
			}
		}

		return true
	})

	return out
}

// nonLiteralOption marks a `set` word whose value is not statically known, so the allowlist cannot
// read it at all. It is a distinct sentinel because it needs different wording in the error.
const nonLiteralOption = "\x00non-literal"

// unmodeledSetOption reports a `set` option outside the modeled set.
//
// `set` is the one builtin here that must stay usable — the real signing block opens with
// `set -euo pipefail` — so it is filtered by option rather than rejected outright. Only the options
// this guard reasons about are accepted: errexit is tracked, and nounset/xtrace/pipefail cannot
// affect whether a command runs or what it resolves to. Everything else fails closed, which is what
// catches `-n` (read commands without executing them) and `-t` (exit after one command) without
// having to know in advance that those two were the dangerous ones.
func unmodeledSetOption(lits []string, litOK []bool) (string, bool) {
	const modeledFlags = "eux"

	modeledLong := map[string]bool{
		"errexit": true, "nounset": true, "xtrace": true, "pipefail": true,
	}

	for i := 0; i < len(lits); i++ {
		// A word whose value is not statically known cannot be checked against the allowlist, so it
		// is unmodeled — the same rule this function exists to apply. Returning "modeled" here was
		// the allowlist failing open on exactly the input it cannot read: `set "${OPTS}"` left the
		// option unchecked while errexitToggle conservatively recorded errexit as OFF, which made
		// parseExecuted skip every later command — including a second `workload push`.
		if !litOK[i] {
			return nonLiteralOption, true
		}

		word := lits[i]

		// `--` and `-` end option parsing; every later word is a positional parameter.
		if word == "--" || word == "-" {
			return "", false
		}

		if !strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "+") {
			// A non-option word is a positional parameter, not a mode change.
			return "", false
		}

		flags := word[1:]

		// `o` takes the NEXT word as its long option name, and it may be bundled — `set -euo pipefail`
		// is `-e -u -o pipefail`. Only a trailing `o` consumes the argument; `set -o` with nothing
		// after it lists the options rather than setting one.
		if strings.HasSuffix(flags, "o") {
			flags = flags[:len(flags)-1]

			if i+1 < len(lits) {
				if !litOK[i+1] || !modeledLong[lits[i+1]] {
					return word + " " + lits[i+1], true
				}

				i++
			}
		}

		for _, flag := range flags {
			if !strings.ContainsRune(modeledFlags, flag) {
				return word, true
			}
		}
	}

	return "", false
}

func declaredNames(file *syntax.File) map[string]bool {
	out := declaredFunctions(file)

	syntax.Walk(file, func(node syntax.Node) bool {
		call, isCall := node.(*syntax.CallExpr)
		if !isCall || len(call.Args) < 2 {
			return true
		}

		if name, ok := literalOf(call.Args[0]); !ok || name != "alias" {
			return true
		}

		for _, arg := range call.Args[1:] {
			definition, ok := literalOf(arg)
			if !ok {
				continue
			}

			// `alias -p` lists aliases rather than defining one; only a NAME=VALUE word binds.
			if name, _, isBinding := strings.Cut(definition, "="); isBinding && name != "" {
				out[name] = true
			}
		}

		return true
	})

	return out
}

// declaredFunctions returns every name the script defines as a shell function.
//
// This is a third axis, after "does it run" (grammar) and "does its failure matter" (errexit): what
// the words actually resolve to. Bash looks up functions before PATH, so a script can define
// `cosign() { true; }` and then issue a textually perfect, failure-propagating, top-level
// `cosign sign … "${REF}"` that produces no signature.
//
// The whole script is searched rather than the statements before the call, and a declaration nested
// inside a conditional counts too. Deciding whether a nested declaration executed is the reachability
// analysis this guard deliberately does not attempt, so it fails closed: a required operation whose
// name is defined as a function anywhere in the same block is not provably the real program. The
// remedy is in the error message — name the wrapper something else, or invoke the real command.
func declaredFunctions(file *syntax.File) map[string]bool {
	out := map[string]bool{}

	syntax.Walk(file, func(node syntax.Node) bool {
		if decl, isDecl := node.(*syntax.FuncDecl); isDecl && decl.Name != nil {
			out[decl.Name.Value] = true
		}

		return true
	})

	return out
}

// errexitToggle reports the new errexit state a `set` command establishes, if it establishes one.
//
// Both spellings count (`set -e` / `set -o errexit`), in either direction, and a combined form such
// as `set -euo pipefail` carries the flag among others.
//
// The words are read AFTER quote removal, because that is the form bash interprets. `set '+e'`,
// `set "+e"` and `set \+e` all disable errexit exactly as `set +e` does, while their source spellings
// match no flag pattern — so comparing source text here silently left errexit tracked as enabled and
// accepted a required operation whose failure could no longer fail the step.
//
// A `set` whose flags are not statically known (`set "${OPTS}"`) yields "errexit off", the
// conservative direction: the guard then cannot prove any later operation gates the step, and says
// so, rather than assuming the safe case.
// terminatesShell reports whether a command ends this shell, so nothing written after it executes.
//
// `exit` returns from the script and `exec <cmd>` replaces the shell image, and in both cases every
// later top-level statement is unreachable. Crediting those statements as executed was a bypass with
// a clean-looking step: `digest=${DIGEST} >> "$GITHUB_OUTPUT"; exit 0; cosign sign …` exports the
// correct digest, exits zero, and satisfied a scan that then recorded the unreachable `cosign sign`
// as having run — so the attestations covered the right digest while the artifact was released
// unsigned.
//
// `exec` with NO command is deliberately excluded: that form only applies redirections to the
// current shell and execution continues past it, so treating it as terminating would blind the guard
// to everything after an ordinary `exec >log`.
//
// The wrapper prefix is stripped first, because `builtin exit 0` and `command exec …` terminate
// exactly as the bare forms do. An unreadable command word yields no termination, which keeps this
// from swallowing the rest of a script the guard cannot actually read.
func terminatesShell(cmd executedCommand) bool {
	start, executes := commandStart(cmd.lits, cmd.litOK)
	if !executes || start >= len(cmd.lits) || !cmd.litOK[start] {
		return false
	}

	switch cmd.lits[start] {
	case "exit":
		return true
	case "exec":
		// Only a form that names a PROGRAM replaces the shell. Counting words was not the same
		// question: `exec -c` carries a word but names nothing, so bash continues — and treating
		// it as terminating stopped the scan and hid every later command, including an extra
		// `workload push`. Options are skipped; anything unreadable returns false, which keeps the
		// rest of the step visible rather than trusting it.
		return execNamesProgram(cmd, start)
	default:
		return false
	}
}

// execNamesProgram reports whether an `exec` call names a program to replace the shell with.
//
// Fail-closed direction matters here and is the opposite of the usual one: claiming the shell ends
// SUPPRESSES the remainder of the scan, so an unreadable word must mean "keep looking", never
// "stop". Options (`-c`, `-l`, `-a NAME`) are skipped; `--` ends them.
func execNamesProgram(cmd executedCommand, start int) bool {
	for i := start + 1; i < len(cmd.lits); i++ {
		if !cmd.litOK[i] {
			return false
		}

		word := cmd.lits[i]

		if word == "--" {
			return i+1 < len(cmd.lits) && cmd.litOK[i+1]
		}

		if strings.HasPrefix(word, "-") {
			// `-a` takes the replacement argv[0] as a separate word; skip that too.
			if word == "-a" {
				i++
			}

			continue
		}

		return true
	}

	return false
}

func errexitToggle(cmd executedCommand) (bool, bool) {
	// `builtin set +e` and `command set +e` run `set` exactly as a bare `set` does, so the wrapper
	// must be stripped before the name is compared. Reading only the first word credited errexit as
	// still ENABLED across a prefixed `set +e` — the dangerous direction, since the guard then
	// believes a later failure would fail the step when it no longer does.
	start, executes := commandStart(cmd.lits, cmd.litOK)
	if !executes || start >= len(cmd.lits) || !cmd.litOK[start] ||
		cmd.lits[start] != "set" || len(cmd.lits)-start < 2 {
		return false, false
	}

	for i := start + 1; i < len(cmd.lits); i++ {
		if !cmd.litOK[i] {
			return false, true
		}
	}

	state, found := false, false

	for i := start + 1; i < len(cmd.lits); i++ {
		word := cmd.lits[i]

		// `--` and a bare `-` end option parsing and assign every remaining word to the positional
		// parameters (bash `help set`). Scanning past them read `set -- -e` as errexit being
		// restored, so a signing block could turn failure propagation OFF, set positionals, and
		// still be credited with a gate that no longer fails the step — the guard reporting the
		// safe state precisely where the unsafe one holds.
		if word == "--" || word == "-" {
			break
		}

		switch {
		case strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--") && strings.Contains(word, "e"):
			state, found = true, true
		case strings.HasPrefix(word, "+") && strings.Contains(word, "e"):
			state, found = false, true
		case (word == "-o" || word == "+o") && i+1 < len(cmd.lits) && cmd.lits[i+1] == "errexit":
			state, found = word == "-o", true
			i++
		}
	}

	return state, found
}

// errexitAtStart reports whether the step begins with failure propagation on.
//
// GitHub runs `shell: bash` as `bash --noprofile --norc -eo pipefail`, so errexit is on before the
// script's first line. Any other shell makes no such promise — and a step may still turn it on
// explicitly, which the toggle tracking above picks up, so this is a starting value rather than a
// verdict.
func errexitAtStart(s step) bool {
	return s.Shell == "" || s.Shell == "bash"
}

func newExecutedCommand(src string, call *syntax.CallExpr) executedCommand {
	cmd := executedCommand{
		words: make([]string, 0, len(call.Args)),
		lits:  make([]string, 0, len(call.Args)),
		litOK: make([]bool, 0, len(call.Args)),
	}

	for _, arg := range call.Args {
		cmd.words = append(cmd.words, sourceOf(src, arg.Pos(), arg.End()))

		lit, ok := literalOf(arg)
		cmd.lits = append(cmd.lits, lit)
		cmd.litOK = append(cmd.litOK, ok)
	}

	return cmd
}

// name returns the command's executable as bash resolves it, after quote removal.
//
// Reading the source form here would let `'cosign' sign …` — a perfectly ordinary invocation of the
// real program — fail to match, which is the over-tightening direction of the same defect that makes
// `set '+e'` slip through.
func (c executedCommand) name() (string, bool) {
	if len(c.lits) == 0 || !c.litOK[0] {
		return "", false
	}

	return c.lits[0], true
}

// assignmentsTo returns every assignment to name anywhere in run, at any nesting depth.
//
// Depth is deliberately ignored here, unlike for the required OPERATIONS. Those must be top-level
// because a conditional one may simply not run; an assignment is different — what matters is that no
// value of this variable can come from somewhere untrustworthy. Restricting it to the top level
// would reject a plain retry loop around the digest resolution, which is correct code, and a guard
// that fails correct input gets suppressed rather than fixed.
//
// Here-document bodies contain no commands, so a resolution "found" inside one is not found at all.
func assignmentsTo(run, name string) []assignment {
	file, err := syntax.NewParser().Parse(strings.NewReader(run), "")
	if err != nil {
		return nil
	}

	var out []assignment

	collect := func(assigns []*syntax.Assign) {
		for _, assign := range assigns {
			if assign.Name == nil || assign.Value == nil || assign.Name.Value != name {
				continue
			}

			out = append(out, assignment{
				text:                 sourceOf(run, assign.Value.Pos(), assign.Value.End()),
				consumesPublishedTag: valueResolvedFrom(assign.Value, publishedTagVar, declaredNames(file)),
			})
		}
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		switch typed := node.(type) {
		case *syntax.CallExpr:
			collect(typed.Assigns)

			// A builtin can write a variable whose name it takes as an ARGUMENT, which the parser
			// models as ordinary words rather than CallExpr.Assigns. `printf -v REF '%s' '<stale>'`
			// was therefore invisible here: a correct resolution could be followed by an unseen
			// overwrite, and both consumers — which require EVERY assignment to qualify — passed
			// while cosign signed the overwritten value.
			if paramExpAssignsTo(typed, name) {
				out = append(out, assignment{text: "", consumesPublishedTag: false})
			}

			if builtinWritesTo(typed, name) {
				out = append(out, assignment{
					// Deliberately opaque. Both consumers compare against an exact required form or
					// demand a resolved value, so an unreadable write fails each of them: the guard
					// reports "cannot prove this" rather than guessing what the builtin wrote.
					text:                 "",
					consumesPublishedTag: false,
				})
			}
		// `export`, `declare`, `local`, `readonly` and `typeset` assign too, and the parser models
		// them as a DeclClause rather than a CallExpr. Reading only CallExpr made
		// `declare -g DIGEST="sha256:…"` invisible, so a constant could silently override a correct
		// resolution — and an action that legitimately exported REF would have been rejected for
		// having no assignment at all.
		case *syntax.DeclClause:
			collect(typed.Args)

			// A NAMEREF is the same overwrite one level of indirection out. `declare -n target=REF`
			// makes every later `target=…` write REF, but the collector above only matches
			// assignments whose literal name is REF — so the one correct assignment satisfied the
			// validator while cosign signed whatever was written through the alias.
			if declaresNamerefTo(typed, name) {
				out = append(out, assignment{text: "", consumesPublishedTag: false})
			}

		// A `for`/`select` clause BINDS its loop variable, and bash leaves the final value in scope
		// after the loop ends. `for REF in <stale-reference>; do :; done` therefore overwrites REF as
		// surely as an assignment does, but the parser models the binding as a WordIter rather than
		// an Assign or a DeclClause, so neither collector above saw it: the validator read only the
		// one correct assignment while cosign signed the stale value the loop left behind.
		//
		// Recorded as an unprovable write, like a builtin destination. The loop's last value depends
		// on the word list and on whether the body runs at all, so there is no single text this could
		// honestly attribute — and both consumers require EVERY assignment to qualify, so an opaque
		// entry makes the guard report that it cannot prove this rather than guess.
		case *syntax.ForClause:
			if iter, isWordIter := typed.Loop.(*syntax.WordIter); isWordIter &&
				iter.Name != nil && iter.Name.Value == name {
				out = append(out, assignment{text: "", consumesPublishedTag: false})
			}
		}

		return true
	})

	return out
}

// declarationWordsWriteTo decodes a declaration whose leading word carried a `builtin`/`command`
// wrapper, which stops the parser modelling it as a DeclClause. It covers both shapes the unprefixed
// path already handles: a direct assignment (`export REF=…`) and a nameref alias (`declare -n
// target=REF`), and it errs toward reporting a write when a word is unreadable.
func declarationWordsWriteTo(lits []string, litOK []bool, name string) bool {
	nameref := false

	for i, word := range lits {
		if !litOK[i] {
			return true
		}

		if strings.HasPrefix(word, "-") && word != "--" {
			if strings.Contains(word, "n") {
				nameref = true
			}

			continue
		}

		lhs, rhs, isAssign := strings.Cut(word, "=")
		if !isAssign {
			continue
		}

		// `export REF=…` writes REF directly; `declare -n target=REF` makes target an alias FOR REF,
		// so there the protected name appears on the right.
		if lhs == name || (nameref && namerefBase(rhs) == name) {
			return true
		}
	}

	return false
}

// namerefBase strips an array subscript from a nameref target.
//
// Bash accepts a subscript on a nameref target, and on a scalar `REF[0]` denotes REF itself — so
// `declare -n target=REF[0]` followed by a write through target assigns REF. Comparing the raw
// target misses that.
//
// Stripping can only ever ADD detections, never remove one: it widens what counts as aliasing the
// protected name, which is the direction this collector already errs in. A target that is genuinely
// a distinct array (`OTHER[0]`) reduces to `OTHER` and still does not match the protected name.
func namerefBase(target string) string {
	base, _, found := strings.Cut(target, "[")
	if !found {
		return target
	}

	return base
}

// declaresNamerefTo reports whether decl creates a nameref aliasing the protected variable.
//
// It does not try to follow the alias. Tracking every later write through an arbitrary alias name is
// a dataflow problem, and getting it subtly wrong here is worse than refusing: the whole point of
// this collector is that the CONSUMERS require every assignment to qualify, so recording the
// aliasing itself as an unprovable write is both sound and small.
//
// The alias TARGET is what matters, not the alias name — `declare -n anything=REF` is a handle on
// REF whatever it is called. A nameref whose target is not readable is also refused, since it could
// be the protected name assembled at run time.
func declaresNamerefTo(decl *syntax.DeclClause, name string) bool {
	if decl.Variant == nil {
		return false
	}

	switch decl.Variant.Value {
	case "declare", "typeset", "local":
	default:
		return false
	}

	nameref := false

	for _, assign := range decl.Args {
		// Options arrive as valueless Args, e.g. the `-n` of `declare -n target=REF`.
		if assign.Name == nil && assign.Value != nil {
			if word, readable := literalOf(assign.Value); readable &&
				strings.HasPrefix(word, "-") && strings.Contains(word, "n") {
				nameref = true
			}

			continue
		}

		if !nameref || assign.Value == nil {
			continue
		}

		target, readable := literalOf(assign.Value)
		if !readable || namerefBase(target) == name {
			return true
		}
	}

	return false
}

// paramExpAssignsTo reports whether the call contains an assignment-form parameter expansion that
// writes name — `${DIGEST:=<stale>}` or `${DIGEST=<stale>}`.
//
// These assign as a SIDE EFFECT of expanding, so they produce no Assign, no DeclClause and no
// builtin destination; `unset DIGEST; : "${DIGEST:=sha256:<old>}"` left the collector seeing only
// the earlier valid resolution while bash signed the fallback. Recorded as an opaque write for the
// same reason builtin writes are: both consumers demand a resolved value, so "cannot prove this"
// fails them rather than guessing.
func paramExpAssignsTo(call *syntax.CallExpr, name string) bool {
	assigns := false

	syntax.Walk(call, func(node syntax.Node) bool {
		if assigns {
			return false
		}

		exp, isExp := node.(*syntax.ParamExp)
		if !isExp || exp.Param == nil || exp.Param.Value != name || exp.Exp == nil {
			return true
		}

		if exp.Exp.Op == syntax.AssignUnset || exp.Exp.Op == syntax.AssignUnsetOrNull {
			assigns = true

			return false
		}

		return true
	})

	return assigns
}

// builtinWritesTo reports whether call writes the shell variable name through a builtin that takes
// its destination as an ARGUMENT rather than as an assignment.
//
// Listing the class rather than the one builtin that was reported matters: `printf -v` is not
// special, it is simply the instance a reviewer happened to find. `read`, `mapfile`/`readarray` and
// `getopts` all take a destination name the same way, and `eval` can assemble any assignment at all,
// so fixing only `printf` would leave four more spellings of the same bypass.
//
// Each builtin is decoded in its OWN argument grammar rather than by scanning every word for the
// name. That precision is the point: `printf '%s' "${REF}" >>"${GITHUB_OUTPUT}"` is correct,
// extremely common, and merely READS the variable. A guard that flagged any command mentioning REF
// would reject correct workflows, and a guard people cannot ship past gets deleted, not obeyed. The
// conservative direction is reserved for the destination position, where an unreadable word really
// could be the name being written.
// commandStart returns the index of the word that names the command actually run, after stripping
// any `builtin` / `command` wrappers, and whether that command is EXECUTED at all.
//
// Bash accepts both wrappers in front of a builtin, so `builtin printf -v REF …` and
// `command printf -v REF …` write REF exactly as the bare form does. Every check that keyed on the
// first word — the errexit toggle, builtin write detection, nameref detection — was defeated by a
// single prefix. Stripping in one place is what keeps the three consistent.
//
// `command -v` / `-V` only LOOK UP a command, so they execute nothing and must not be read as a
// write; `command -p` still executes, merely with a default PATH. An unreadable word ends the strip:
// the guard cannot see what it wraps, so the caller's own conservative handling takes over.
func commandStart(lits []string, litOK []bool) (int, bool) {
	i := 0

	for i < len(lits) {
		if !litOK[i] {
			return i, true
		}

		switch lits[i] {
		case "builtin":
			i++

			// `builtin -- set +e` runs `set` exactly as `builtin set +e` does. Advancing only past
			// the wrapper left `--` standing as the apparent command name, so `set` was never
			// recognised and errexit stayed tracked as enabled across a step that had disabled it.
			// `command` already skipped its terminator below; this is the same rule for `builtin`.
			if i < len(lits) && litOK[i] && lits[i] == "--" {
				i++
			}
		case "command":
			i++

			for i < len(lits) && litOK[i] && strings.HasPrefix(lits[i], "-") && lits[i] != "--" {
				if strings.ContainsAny(lits[i], "vV") {
					return i, false
				}

				i++
			}

			if i < len(lits) && litOK[i] && lits[i] == "--" {
				i++
			}
		default:
			return i, true
		}
	}

	return i, true
}

// wordsOf flattens a call's arguments into the literal/readable pair commandStart consumes.
func wordsOf(args []*syntax.Word) ([]string, []bool) {
	lits := make([]string, 0, len(args))
	ok := make([]bool, 0, len(args))

	for _, arg := range args {
		word, readable := literalOf(arg)
		lits = append(lits, word)
		ok = append(ok, readable)
	}

	return lits, ok
}

func builtinWritesTo(call *syntax.CallExpr, name string) bool {
	if len(call.Args) == 0 {
		return false
	}

	lits, litOK := wordsOf(call.Args)

	start, executes := commandStart(lits, litOK)
	if !executes || start >= len(call.Args) || !litOK[start] {
		return false
	}

	command := lits[start]
	args := call.Args[start+1:]

	// A prefixed declaration is not parsed as a DeclClause — `builtin declare -n target=REF` is an
	// ordinary CallExpr — so the declaration family is decoded here from words as well.
	switch command {
	case "declare", "typeset", "local", "export", "readonly":
		return declarationWordsWriteTo(lits[start+1:], litOK[start+1:], name)
	}

	switch command {
	case "printf":
		return printfWritesTo(args, name)
	case "read":
		return readLikeWritesTo(args, name, "adinNptu")
	case "mapfile", "readarray":
		return readLikeWritesTo(args, name, "dnOsuCc")
	case "getopts":
		// `getopts optstring NAME [arg...]`: the first operand is the option string, every later
		// one is a destination or a scanned argument. Comparing all but the first keeps it simple
		// and errs toward reporting a write.
		return operandWritesTo(args, name, 1)
	case "eval":
		return evalWritesTo(args, name)
	}

	return false
}

// printfWritesTo decodes `printf -v NAME …`, including the attached `-vNAME` spelling that an exact
// "-v then the next word" scan would miss.
func printfWritesTo(args []*syntax.Word, name string) bool {
	for i, arg := range args {
		word, readable := literalOf(arg)
		if !readable {
			continue
		}

		if word == "--" {
			return false
		}

		if word == "-v" {
			if i+1 >= len(args) {
				return false
			}

			target, targetReadable := literalOf(args[i+1])

			return !targetReadable || namerefBase(target) == name
		}

		if strings.HasPrefix(word, "-v") && len(word) > 2 {
			return namerefBase(word[2:]) == name
		}
	}

	return false
}

// readLikeWritesTo decodes `read` and `mapfile`/`readarray`, whose destination names are the
// operands left after option parsing. valueOpts lists the single-letter options that consume the
// following word, so an option's ARGUMENT is never mistaken for a destination.
//
// `read -a NAME` is a write like any other: the array name is where the value lands.
func readLikeWritesTo(args []*syntax.Word, name, valueOpts string) bool {
	for i := 0; i < len(args); i++ {
		word, readable := literalOf(args[i])
		if !readable {
			// Unreadable in operand position could be the destination.
			return true
		}

		if word == "--" {
			return operandWritesTo(args[i+1:], name, 0)
		}

		if !strings.HasPrefix(word, "-") || word == "-" {
			return operandWritesTo(args[i:], name, 0)
		}

		// A value-taking option given as `-p prompt` consumes the next word; `-ppromopt` and
		// clustered flags consume nothing further.
		if len(word) == 2 && strings.ContainsRune(valueOpts, rune(word[1])) {
			// `read -a NAME` names its destination in the option's own argument.
			if word == "-a" && i+1 < len(args) {
				target, targetReadable := literalOf(args[i+1])
				if !targetReadable || namerefBase(target) == name {
					return true
				}
			}

			i++
		}
	}

	return false
}

// operandWritesTo reports whether any operand from skip onward names the protected variable. An
// unreadable operand counts, because it could be the destination and the guard cannot prove it is
// not.
func operandWritesTo(args []*syntax.Word, name string, skip int) bool {
	for i, arg := range args {
		if i < skip {
			continue
		}

		word, readable := literalOf(arg)
		if !readable || namerefBase(word) == name {
			return true
		}
	}

	return false
}

// evalWritesTo reports whether an eval could assign the protected variable. Its argument is a
// program, so a readable one is searched for an assignment to the name and an unreadable one is
// treated as a write: eval assembling text the guard cannot see is exactly the case it must not
// certify.
func evalWritesTo(args []*syntax.Word, name string) bool {
	for _, arg := range args {
		word, readable := literalOf(arg)
		if !readable {
			return true
		}

		if strings.Contains(word, name+"=") {
			return true
		}
	}

	return false
}

// sourceOf returns the source text a node spans. Comparing what was WRITTEN keeps the required
// argument checks exact: `"${REF}"` and `"${REF_TAG}"` expand to different things at run time, and
// the guard has to distinguish them without expanding anything.
func sourceOf(src string, from, to syntax.Pos) string {
	start, end := int(from.Offset()), int(to.Offset())
	if start < 0 || end > len(src) || start > end {
		return ""
	}

	return src[start:end]
}

// valueResolvedFrom reports whether a value is produced by RUNNING something that reads name.
//
// Both halves are load-bearing, and each closes a different bypass:
//
//   - The value must come from a command substitution, so a digest that was written down cannot
//     pass as one that was resolved. Depth matters here because `DIGEST="$(…)"` wraps the
//     substitution in a *syntax.DblQuoted; quoting one is BETTER practice than leaving it bare, so
//     a shallow scan rejected the more careful spelling of correct code.
//   - name must be EXPANDED as an argument of a command that substitution runs — not merely
//     present in its source text. A source span covers the comments inside the substitution, so
//     `DIGEST="$(printf '%s' 'sha256:…'  # ${REF_TAG}
//     )"` satisfied a substring test while bash expanded nothing: cosign and both attestations then
//     cover an OLDER manifest in full while the tag this run published ships unsigned. Requiring an
//     argument also settles the neighbouring shape, `"${REF_TAG}$(…constant…)"`, where the tag is
//     genuinely expanded but the substitution that decides the value never sees it.
//
// This is the same distinction the parser already draws for the required OPERATIONS — a mention is
// not an execution — applied one level down to the values they consume.
//
// Reading the parsed expansion rather than the written `${REF_TAG}` also stops the guard enforcing a
// spelling it has no stake in: `$REF_TAG` is the same expansion, and rejecting it would fail correct
// code.
//
// The tag must be read by the statement whose output BECOMES the value, not merely by something
// somewhere inside the substitution — see consumes for why that distinction is load-bearing.
func valueResolvedFrom(word *syntax.Word, name string, shadowed map[string]bool) bool {
	resolved := false

	syntax.Walk(word, func(node syntax.Node) bool {
		subst, isSubst := node.(*syntax.CmdSubst)
		if !isSubst {
			return !resolved
		}

		if len(subst.Stmts) > 0 && consumes(subst.Stmts[len(subst.Stmts)-1], name, shadowed) {
			resolved = true
		}

		return !resolved
	})

	return resolved
}

// consumes reports whether the statement whose output becomes the substitution's value reads name.
//
// Scoping this to the LAST statement is what ties the resolution to the RESULT. Accepting the tag
// anywhere inside the substitution let a no-op consume it while a later command printed the value
// that actually became the digest:
//
//	DIGEST="$( : "${REF_TAG}"; printf '%s' 'sha256:…' )"
//
// Cosign and both attestations then cover that constant in full while the tag this run published
// ships unsigned — a bypass with a GREEN result, which is the worst shape a security guard can have.
//
// The accepted shape is ENUMERATED rather than searched. Recursing through every nested call was
// still too loose: a branch that never runs is part of the last statement, so
//
//	DIGEST="$( if false; then crane digest "${REF_TAG}"; else printf '%s' 'sha256:…'; fi )"
//
// contained a consuming call while bash returned the constant. Anything whose output depends on
// which branch or iteration executes cannot be bound without evaluating it, so it is rejected
// outright — that is the conservative direction, and it is what makes this terminal instead of one
// more round.
//
// A single call is the ONLY construct whose output can be bound without evaluating the script, which
// is why it is the whole accepted set. Everything else was tried and each admitted a bypass:
//
//   - a compound (`if`/`for`/`while`/`case`/subshell/`&&`) emits the output of whichever branch or
//     iteration ran, so a resolver in an untaken branch satisfied a search while bash returned a
//     constant;
//   - a PIPELINE is no better, though it looks it. `crane digest "${REF_TAG}" | printf '%s' 'sha256:…'`
//     is a pipeline of plain calls whose first stage reads the tag — and `printf` never reads stdin,
//     so the constant is what becomes DIGEST. "Later stages only reshape the resolver's output" reads
//     as obviously true and is not a property shell syntax enforces.
//
// A retry belongs around the ASSIGNMENT (`for … do DIGEST=$(…) && break; done`), which is unaffected:
// assignmentsTo finds it at any depth, so the idiomatic spelling still passes.
func consumes(stmt *syntax.Stmt, name string, shadowed map[string]bool) bool {
	call, isCall := stmt.Cmd.(*syntax.CallExpr)
	if !isCall || len(call.Args) == 0 {
		return false
	}

	// Taking the tag as an argument does not mean the output depends on it. `printf` accepts extra
	// arguments and ignores every one of them when the format string carries no conversion, so
	//
	//	DIGEST=$(printf 'sha256:<old-digest>' "${REF_TAG}")
	//
	// is a single direct call that reads the published tag and returns a constant — the same bypass
	// as the pipeline and the untaken branch, in the one shape that survived narrowing the
	// STRUCTURE. Structure alone cannot distinguish them: what separates a resolver from `printf` is
	// what the program does, not how the call is written.
	//
	// So the executable is pinned to the small set whose documented contract is "resolve a reference
	// to a digest". This is an allowlist and it is deliberately not exhaustive — a tool missing from
	// it fails loudly with the list in the message, which is a one-line, reviewed edit. The
	// alternative direction, rejecting known no-ops, has been tried on this predicate five times and
	// each round found another one.
	exec, known := literalOf(call.Args[0])
	if !known || !digestResolvers[exec] {
		return false
	}

	// Allow-listing the NAME only binds the value if the name still reaches that program. bash looks
	// up functions before $PATH, so `docker() { printf 'sha256:<old>'; }` makes this call return a
	// constant while reading as a genuine resolve. Top-level required commands were already checked
	// against declaredNames; the resolver is the same question about a different call.
	if shadowed[exec] {
		return false
	}

	for _, arg := range call.Args[1:] {
		if expands(arg, name) {
			return true
		}
	}

	return false
}

// digestResolvers are the executables accepted as resolving a reference to a digest.
//
// Membership is about the program's contract, not about which one this repo happens to use: the
// deploy runs `docker buildx imagetools inspect`, and switching to crane, skopeo, oras, regctl or
// cosign is a legitimate refactor that must not fail CI. What is excluded is everything whose output
// is unrelated to its arguments — `printf`, `echo`, `cat`, `true` — which is the whole bypass.
var digestResolvers = map[string]bool{
	"cosign": true,
	"crane":  true,
	"docker": true,
	"oras":   true,
	"regctl": true,
	"skopeo": true,
}

// expands reports whether a word contains a parameter expansion of name.
//
// A comment's text is never parsed into an expansion node, and neither is a single-quoted string, so
// walking the tree distinguishes what bash will substitute from what merely reads that way. Both
// spellings are the same expansion: `$REF_TAG` is a short ParamExp, `${REF_TAG}` a braced one.
func expands(word *syntax.Word, name string) bool {
	found := false

	syntax.Walk(word, func(node syntax.Node) bool {
		param, isParam := node.(*syntax.ParamExp)
		if isParam && param.Param != nil && param.Param.Value == name {
			found = true
		}

		return !found
	})

	return found
}

// viaKsail accepts the authenticated wrapper the composite publishes through. `workload push` is a
// SUBCOMMAND, so it is legitimately preceded by a command — the real step runs
// `./scripts/run-ksail-prod-with-pull-auth.sh workload push` — which is why the executable cannot
// simply be required to be the operation itself, the way it can for cosign. It is restricted to
// ksail or a script rather than pinned to one path, so renaming the wrapper stays a legal refactor.
func viaKsail(exec string) bool {
	return exec == "ksail" || strings.HasSuffix(exec, ".sh")
}

// interpreters run a script named as their ARGUMENT, so the executable is not what executes.
var interpreters = map[string]bool{
	"bash": true, "sh": true, "dash": true, "ksh": true, "zsh": true,
}

// scriptTarget returns the program a command actually runs, looking through an interpreter, and the
// index its own arguments start at.
//
// `bash ./scripts/run-ksail-prod-with-pull-auth.sh workload push` publishes exactly as the direct
// invocation does, but the executable reads as `bash`, so a matcher keyed on the executable omitted
// the operation entirely. That omission is fail-OPEN in the direction that matters most: the extra
// push it hides is the one this guard exists to reject, since a second publish AFTER the evidence
// steps releases bytes nothing covers.
//
// Only the leading interpreter is stripped, and only when it names a script. An interpreter carrying
// inline code (`bash -c …`) names no script and runs text this file never parses, so it is rejected
// by unmodeledShellState rather than guessed at here.
// The `command`/`builtin` prefixes are stripped first. They execute the named command with its
// arguments, so `command ./scripts/run-ksail-prod-with-pull-auth.sh workload push` publishes exactly
// as the bare form does — but reading argv[0] returns `command`, which matches no wrapper and hid the
// operation. commandStart already decodes those prefixes for the errexit tracking; this uses the same
// decoder rather than a second, differently-wrong one.
func scriptTarget(cmd executedCommand) (string, bool, int) {
	start, executes := commandStart(cmd.lits, cmd.litOK)
	if !executes || start >= len(cmd.lits) || !cmd.litOK[start] {
		return "", false, 0
	}

	exec := cmd.lits[start]
	if !interpreters[exec] {
		return exec, true, start
	}

	// Skip the interpreter's own options to reach the script operand.
	for i := start + 1; i < len(cmd.lits); i++ {
		if !cmd.litOK[i] {
			return "", false, 0
		}

		if strings.HasPrefix(cmd.lits[i], "-") {
			continue
		}

		return cmd.lits[i], true, i
	}

	return "", false, 0
}

// ksailWorkload matches a step that runs `workload <op>` through that wrapper.
//
// The executable matters as much as the words: `ksail workload push` and `echo workload push` have
// identical shape, so a rule reading only the operation's words would accept a step that merely
// prints it.
func ksailWorkload(op string) func(step) bool {
	return func(s step) bool {
		for _, cmd := range parseExecuted(s.Run, errexitAtStart(s)) {
			exec, known, from := scriptTarget(cmd)
			if !known || !viaKsail(exec) {
				continue
			}

			for i := from + 1; i+1 < len(cmd.lits); i++ {
				if cmd.litOK[i] && cmd.lits[i] == "workload" && cmd.litOK[i+1] && cmd.lits[i+1] == op {
					return true
				}
			}
		}

		return false
	}
}

// cosignSignCommands returns every executed `cosign sign` invocation in run.
func cosignSignCommands(s step) []executedCommand {
	var out []executedCommand

	for _, cmd := range parseExecuted(s.Run, errexitAtStart(s)) {
		exec, known := cmd.name()
		if known && exec == "cosign" && len(cmd.lits) >= 2 && cmd.lits[1] == "sign" {
			out = append(out, cmd)
		}
	}

	return out
}

// signFlags are the options `cosign sign` may carry here, as an ALLOWLIST.
//
// Cosign has options that make a successful, correctly-referenced signing command publish nothing:
// `--upload=false` generates the signature and never writes it to the registry, so the invocation
// contains the exact required reference, exits zero, and leaves the artifact unsigned. Rejecting
// that one flag would be a blocklist over an interface this repo does not control — the next option
// with the same effect arrives with the next cosign release and is silently accepted.
//
// So the accepted set is enumerated instead: the two flags the deploy actually uses. An unfamiliar
// flag fails with the list in the message, which is a deliberate, reviewed one-line edit here rather
// than a silent change in what the evidence means.
var signFlags = map[string]bool{
	"--yes":       true,
	"-y":          true,
	"--recursive": true,
	"-r":          true,
}

// unexpectedSignFlag returns the first option in the invocation that is not on the allowlist.
//
// The reference argument is exempt: it is required to be `"${REF}"` and is asserted separately, and
// it is the one word here that is legitimately not statically known.
//
// Any OTHER word whose value is not statically known is reported, because a word that expands at run
// time can supply an option this allowlist would otherwise reject — `cosign sign ${EXTRA} "${REF}"`
// must not be a way around the list. A literal positional is left alone; it is not an option, and
// cosign's own argument handling is not this guard's business.
func unexpectedSignFlag(cmd executedCommand) (string, bool) {
	// Skip the executable and the `sign` subcommand.
	for i := 2; i < len(cmd.lits); i++ {
		if cmd.words[i] == signRefArg {
			continue
		}

		if !cmd.litOK[i] {
			return cmd.words[i], true
		}

		word := cmd.lits[i]
		if !strings.HasPrefix(word, "-") {
			continue
		}

		// `--flag=value` and `--flag` are the same option.
		if name, _, hasValue := strings.Cut(word, "="); hasValue {
			word = name
		}

		if !signFlags[word] {
			return word, true
		}
	}

	return "", false
}

func runsCosignSign(s step) bool {
	return len(cosignSignCommands(s)) > 0
}

func usesPrefix(prefix string) func(step) bool {
	return func(s step) bool { return strings.HasPrefix(s.Uses, prefix) }
}

// The contract is a partial order, not a chain: publish first, then the
// evidence in any order among itself, then the release. Modelling it as a
// straight sequence would additionally forbid swapping the two attestations,
// which is a free choice — failing CI on a legitimate reordering trains people
// to treat this check as noise.
//
// The SBOM generator is deliberately absent: it produces a file rather than
// publishing anything, so its position carries no guarantee.
var (
	// publish makes new bytes reachable and must come first — signing or
	// attesting before it would cover the previous artifact.
	publish = marker{label: "publish the manifests (`workload push`)", match: ksailWorkload("push")}

	// sign is named separately because its step is inspected again below, for
	// the digest reference. Locating it by value rather than by matching its
	// label keeps that second lookup correct when the wording changes.
	sign = marker{label: "sign the published digest (`cosign sign`)", match: runsCosignSign}

	// release is the step that advances what production can see.
	release = marker{label: "tell Flux to reconcile (`workload reconcile`)", match: ksailWorkload("reconcile")}
)

// evidenceMarkers is what must exist before production may look. Order among these is unconstrained.
//
// The attestation markers are built from the signing step's id rather than declared as constants,
// because what makes an attestation evidence is that its subject is the digest THIS run resolved and
// signed. Pinning the subject to a literal would break the moment the step is renamed; binding it to
// the signing step's output keeps the two moving together.
func evidenceMarkers(signStepID string) []marker {
	coversSubject := attestsSignedDigest(signStepID)

	attests := func(label, prefix string) marker {
		return marker{
			label: label,
			match: func(s step) bool { return usesPrefix(prefix)(s) && coversSubject(s) },
			hint: fmt.Sprintf(
				"This step is matched on its action reference AND on what it attests, because the"+
					" reference alone does not say which artifact the evidence covers. Check that it"+
					" still uses `%s…`, that `subject-digest` is `${{ steps.%s.outputs.digest }}`"+
					" (the digest this run resolved and signed — a literal or an older digest"+
					" produces real evidence about the wrong artifact), and that"+
					" `push-to-registry` is `true`, without which the attestation is generated but"+
					" never reaches the registry",
				prefix, signStepID),
		}
	}

	return []marker{
		sign,
		attests("attest the SBOM (`actions/attest`)", "actions/attest@"),
		attests(
			"attest build provenance (`actions/attest-build-provenance`)",
			"actions/attest-build-provenance@",
		),
	}
}

// attestsSignedDigest reports whether an attestation step covers the digest this run resolved and
// publishes the result where a consumer can find it.
//
// The action reference alone says only which program runs. `subject-digest` is a free input, so an
// attestation step can keep its position, its pinned `uses:`, and its green result while covering an
// OLDER digest — evidence that is genuine, verifiable, and about the wrong artifact. That is the
// same failure the DIGEST provenance check exists to prevent, one layer up.
//
// `push-to-registry` matters for the same reason `--upload` does on the signature: an attestation
// that is generated but never pushed leaves the registry with no evidence attached, while the step
// succeeds.
//
// The subject must be bound to the signing step's OUTPUT rather than to any particular text, so
// renaming the step is a legal refactor as long as the binding follows it.
func attestsSignedDigest(signStepID string) func(step) bool {
	wantDigest := fmt.Sprintf("${{ steps.%s.outputs.digest }}", signStepID)

	return func(s step) bool {
		if strings.TrimSpace(s.With["subject-digest"]) != wantDigest {
			return false
		}

		return strings.TrimSpace(s.With["push-to-registry"]) == "true"
	}
}

// digestRef is the form the signing step must use. Signing the mutable tag
// instead would let a concurrent deploy move it between resolve and sign, so
// the signature would cover bytes this run never published.
const (
	// digestRefValue is the value REF must be assigned, as written.
	digestRefValue = `"ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`
	// digestRef is the whole assignment, used in error messages.
	digestRef = `REF=` + digestRefValue
	// publishedTagVar is the variable holding the tag this run just pushed. DIGEST must be derived
	// from it. This is the NAME, not a spelling of the expansion: the check reads the parsed
	// expansion, so `$REF_TAG` and `${REF_TAG}` both qualify.
	publishedTagVar = `REF_TAG`
	// publishedTagRef is that expansion as an operator would write it, for error messages.
	publishedTagRef = `${` + publishedTagVar + `}`
)

// indicesOf returns the position of EVERY step matching m, in file order.
//
// Every occurrence matters, not just the first. A composite that pushes, signs,
// reconciles and then pushes again satisfies the contract on its first push
// while republishing bytes nothing signed or attested — so locating one step of
// each kind and stopping would validate exactly the window this guard exists to
// close.
func indicesOf(steps []step, m marker) []int {
	var at []int

	for i, s := range steps {
		if m.match(s) {
			at = append(at, i)
		}
	}

	return at
}

// signRefArg is the argument the cosign invocation must consume. Asserting the
// assignment of digestRef alone is not enough: a step can resolve the digest
// into REF and still sign "${REF_TAG}", leaving the assignment dead and the
// signature covering whatever the mutable tag resolves to at sign time.
const signRefArg = `"${REF}"`

// signsTheResolvedDigest reports whether every cosign invocation in run signs
// the reference built from the resolved digest.
//
// The reference must be an ARGUMENT, which is why this reads parsed words rather than the line. A
// substring test cannot tell an argument from a comment, so
// `cosign sign --yes --recursive "${REF_TAG}" # "${REF}"` satisfied it while bash passed only the
// mutable tag — restoring the exact tag-resolution race the check exists to prevent.
func signsTheResolvedDigest(s step) bool {
	signing := cosignSignCommands(s)

	// No executed invocation at all means nothing signs, whatever the text contains.
	if len(signing) == 0 {
		return false
	}

	for _, cmd := range signing {
		if !slices.Contains(cmd.words, signRefArg) {
			return false
		}
	}

	return true
}

// assignsResolvedRef reports whether EVERY assignment to REF builds it from the resolved digest.
//
// "Every" rather than "some": a later assignment overwrites an earlier one, so accepting the
// presence of one correct assignment would let a second, wrong one decide what cosign actually
// signs.
func assignsResolvedRef(run string) bool {
	refs := assignmentsTo(run, "REF")
	if len(refs) == 0 {
		return false
	}

	for _, assigned := range refs {
		if assigned.text != digestRefValue {
			return false
		}
	}

	return true
}

// resolvesDigestFromPublishedTag reports whether run derives DIGEST by inspecting the tag it just
// published, rather than naming a digest outright.
//
// Without this, DIGEST's provenance was unbound: every ordering check still held while a constant
// digest for an OLDER manifest was signed and attested in full, and reconciliation released the
// newly pushed — and unsigned — `latest`. That is the most dangerous shape the guard can miss,
// because the evidence it produces is genuine, just about the wrong artifact.
//
// The requirement is that the value is RESOLVED from the published tag, not that one specific tool
// resolves it: `docker buildx imagetools`, `crane` and a cosign equivalent are all legitimate, and a
// guard that failed a correct switch between them would be suppressed rather than fixed.
// EVERY assignment must qualify, for the same reason as REF: one resolved assignment sitting beside
// a constant one proves nothing about which value cosign ends up signing.
func resolvesDigestFromPublishedTag(run string) bool {
	digests := assignmentsTo(run, "DIGEST")
	if len(digests) == 0 {
		return false
	}

	for _, assigned := range digests {
		if !assigned.consumesPublishedTag {
			return false
		}
	}

	return true
}

// mustNotSkipIndependently rejects a required step whose condition lets it be skipped while a
// release still runs.
//
// The comparison is against EVERY release, not just the binding one for ordering. Checking only the
// first left a gap with the same shape as the one indicesOf closes for ordering: evidence guarded by
// the same never-firing condition as a first reconcile looked correctly coupled, while a second,
// unconditional reconcile released the artifact with all of that evidence skipped. A required step
// has to stand or fall with each release, so one it cannot gate is a failure.
func mustNotSkipIndependently(steps []step, m marker, idx []int, releaseIdx []int) error {
	for _, releaseAt := range releaseIdx {
		releaseIf := strings.TrimSpace(steps[releaseAt].If)

		for _, at := range idx {
			stepIf := strings.TrimSpace(steps[at].If)
			if stepIf == "" || stepIf == releaseIf {
				continue
			}

			return fmt.Errorf(
				"the step that must %s carries `if: %s`, which does not match the release step's"+
					" condition (%q) at position %d, so it can be skipped while that release still"+
					" runs.\n"+
					"GitHub skips a false-conditioned step without failing the composite, so the"+
					" ordering above would still hold while production is released without that"+
					" evidence",
				m.label, stepIf, releaseIf, releaseAt+1)
		}
	}

	return nil
}

// statusChecks are the GitHub expression functions that decouple a step from earlier failures.
//
// A step's `if:` is implicitly `success() && <expr>` — UNLESS the expression itself calls a status
// check function, which replaces that default. So `if: github.ref == 'refs/heads/main'` still
// requires every earlier step to have succeeded, while `if: always()` does not, and neither does
// `if: !cancelled()`.
//
// `success(` is on this list even though it reads as the safe one, because the rule above keys on
// the CALL, not on which function is called: writing `success()` explicitly replaces the implicit
// gate just as `always()` does, and `success() || true` is then a release that survives a failed
// evidence step while reading as if it required success. The spellings that are genuinely safe —
// `success()` alone, or `success() && <expr>` — are exactly the ones the implicit gate already
// provides, so rejecting them costs nothing: the remedy is to drop the call and keep `<expr>`.
//
// The entries are lowercase and the condition is lowercased before matching, so a case variant
// (`ALWAYS()`, `Always()`) is caught too. That holds under either reading of GitHub's expression
// parser: if function names resolve case-insensitively then a case variant is a working bypass and
// has to be rejected; if they do not, it is not a spelling any legitimate condition uses, so
// rejecting it costs nothing. Matching only the lowercase spelling is the one choice that is wrong
// in the dangerous direction.
var statusChecks = []string{"always(", "failure(", "cancelled(", "success("}

// mustNotReleaseAfterFailure rejects a release whose condition survives a failed evidence step.
//
// Every ordering guarantee in this file assumes that a failing signature or attestation stops the
// deploy before reconciliation. GitHub provides that for free — but only while the release step
// keeps its implicit `success()`. Add `if: always()` and the sequence still reads correctly, every
// evidence step still precedes the release, and production is released precisely when the evidence
// FAILED, which is the one case all of this exists to prevent.
//
// The condition is rejected outright rather than compared against the evidence steps' conditions:
// matching conditions couple the steps' SKIPPING, not their success, so evidence and release both
// running under `always()` is equally broken.
func mustNotReleaseAfterFailure(steps []step, releaseIdx []int) error {
	for _, at := range releaseIdx {
		condition := strings.TrimSpace(steps[at].If)
		// Strip EVERY whitespace rune, not just U+0020. A YAML literal block scalar keeps its
		// newlines verbatim and a double-quoted scalar resolves `\t`, so `always\n()` and
		// `always\t()` both reach here intact — GitHub evaluates them as calls, while a space-only
		// strip left them unmatched and released production on failed evidence.
		normalized := strings.ToLower(strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}

			return r
		}, condition))

		for _, check := range statusChecks {
			if !strings.Contains(normalized, check) {
				continue
			}

			return fmt.Errorf(
				"the step that must %s carries `if: %s` at position %d, which calls a status check"+
					" function and so replaces the implicit `success()` that couples it to the"+
					" evidence above it.\n"+
					"GitHub skips the remaining evidence steps when signing or attestation fails, but"+
					" a release conditioned this way still runs — releasing production at exactly the"+
					" moment the evidence is known to be missing.\n"+
					"Gate the release on success instead: drop the condition, or express it without"+
					" always()/failure()/cancelled()/success() — an explicit success() call is"+
					" redundant with the implicit gate when it stands alone, so removing it keeps the"+
					" coupling the release needs",
				release.label, condition, at+1)
		}
	}

	return nil
}

// shellStartupEnv are the step-level names that decide how the run block is interpreted or resolved,
// before and while it executes — set from outside the script, so the run block stays textually
// perfect either way.
//
// Bash expands BASH_ENV in a non-interactive shell and reads the resulting file before executing the
// script, so `BASH_ENV: ./setup.sh` with `cosign() { true; }` inside it shadows the required command.
// ENV is the same mechanism under POSIX mode, and SHELLOPTS/BASHOPTS set shell options — including
// noexec — from outside the script.
//
// PATH belongs here for the same reason it is already rejected as an in-script assignment above
// ("PATH decides where every unqualified name resolves"): `env: PATH: /tmp/fake` resolves the
// textually perfect `cosign sign …` to whatever `cosign` sits in that directory. The script-level
// spellings were covered while the step-level one was not, which left the same shadowing reachable
// without touching the script at all.
var shellStartupEnv = []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "PATH"}

// mustModelEveryRunStep rejects an action carrying a construct this guard cannot interpret.
//
// Scope is every run step of the action rather than only the ones matching a marker, because which
// steps match is itself decided by the analysis this protects: a step made unreadable stops matching,
// and "no step appears to sign" would then be reported as a missing step rather than as an
// unreadable one. Both are failures, but only the second names the actual cause.
func mustModelEveryRunStep(steps []step) error {
	for i, s := range steps {
		if s.Run == "" {
			continue
		}

		label := s.Name
		if label == "" {
			label = fmt.Sprintf("position %d", i+1)
		}

		for _, name := range shellStartupEnv {
			if _, set := s.Env[name]; set {
				return fmt.Errorf(
					"step %q sets %s, which decides how the run block is interpreted or resolved from"+
						" outside the script.\n"+
						"Bash reads that startup file, applies those options, or resolves unqualified names"+
						" through that PATH, so a required command can be shadowed by something this check"+
						" never sees while the run block stays textually correct. Configure the step"+
						" without it",
					label, name)
			}
		}

		// A parse failure is the same fail-open shape as an unmodeled construct, for the same reason:
		// parseExecuted yields no commands, which correctly refuses to CREDIT a required operation but
		// silently hides a forbidden one. An unparseable step could therefore carry a second
		// `workload push` after the evidence steps and be reported as satisfying the contract.
		file, err := syntax.NewParser().Parse(strings.NewReader(s.Run), "")
		if err != nil {
			return fmt.Errorf(
				"step %q is not parseable as shell: %w.\n"+
					"Nothing in it can be checked, including whether it publishes again after the"+
					" evidence steps, so it is rejected rather than skipped",
				label, err)
		}

		if construct, unmodeled := unmodeledShellState(file); unmodeled {
			return fmt.Errorf(
				"step %q uses %s, which changes how the shell resolves a command or propagates its"+
					" failure in a way this check does not model.\n"+
					"That makes both halves of the check unreliable: a required operation can look"+
					" executed when it was not, and an extra one can go unseen. This is deliberately"+
					" fail-closed — express the step without that construct, or extend the model in"+
					" unmodeledShellState deliberately",
				label, construct)
		}
	}

	return nil
}

func validate(source []byte) error {
	var parsed action
	if err := yaml.Unmarshal(source, &parsed); err != nil {
		return fmt.Errorf("could not parse as YAML: %w", err)
	}

	steps := parsed.Runs.Steps
	if len(steps) == 0 {
		return errors.New("no runs.steps; this does not look like a composite action")
	}

	// Established before any ordering question, because an unmodeled state change makes every later
	// answer unreliable in BOTH directions: a required operation can be credited when it never ran,
	// and an extra one can go unseen. Reporting it as an error rather than as "this step matches
	// nothing" is what keeps the second half from failing open.
	if err := mustModelEveryRunStep(steps); err != nil {
		return err
	}

	locate := func(m marker) ([]int, error) {
		at := indicesOf(steps, m)
		if len(at) == 0 {
			if m.hint != "" {
				return nil, fmt.Errorf("no step appears to %s.\n%s", m.label, m.hint)
			}

			return nil, fmt.Errorf(
				"no step appears to %s.\n"+
					"This check follows what a step does rather than what it is called, so it also reports"+
					" a command that is present but will not take effect. Check, in order: the command or"+
					" action it matches on is still there; it runs unconditionally at the top level (not"+
					" inside `if`/`&&`/`||`/a pipeline/a function body/a here-document);"+
					" its failure can still fail the step (no `set +e`, no trailing `|| true`, no `&`,"+
					" and `shell: bash`); and its name is not shadowed by a shell function defined in the"+
					" same step, which bash resolves before PATH.\n"+
					"If the step was genuinely renamed or restructured, update the marker here deliberately",
				m.label)
		}

		return at, nil
	}

	// Comparing the LAST occurrence of the earlier kind against the FIRST of the
	// later kind is what makes this hold across every pair: if even the latest
	// publish precedes the earliest piece of evidence, all of them do.
	mustPrecede := func(earlier marker, earlierIdx []int, later marker, laterIdx []int) error {
		earlierAt := earlierIdx[len(earlierIdx)-1]
		laterAt := laterIdx[0]

		if earlierAt < laterAt {
			return nil
		}

		return fmt.Errorf(
			"the deploy must %s BEFORE it can %s, but the steps are in the opposite order "+
				"(positions %d and %d).\n"+
				"Flux resolves the mutable tag on its own schedule, so any step that advances what "+
				"production can see must come after the evidence it depends on is published",
			earlier.label, later.label, earlierAt+1, laterAt+1)
	}

	publishAt, err := locate(publish)
	if err != nil {
		return err
	}

	releaseAt, err := locate(release)
	if err != nil {
		return err
	}

	// Every ordering comparison below assumes a failed evidence step stops the deploy. That is
	// GitHub's default and a condition on the release can remove it, so it is established first —
	// otherwise the whole sequence can hold while production is released from a failed run.
	if err := mustNotReleaseAfterFailure(steps, releaseAt); err != nil {
		return err
	}

	signAt, err := locate(sign)
	if err != nil {
		return err
	}

	// Report a missing `id:` as itself. Without this the marker becomes
	// `${{ steps..outputs.digest }}`, which no attestation can match, and the failure then points at
	// the attestation steps and tells the reader to write that empty expression into them — sending
	// them to the wrong file and the wrong fix. A guard that misnames the defect costs more than one
	// that stays silent.
	signStepID := strings.TrimSpace(steps[signAt[0]].ID)
	if signStepID == "" {
		return fmt.Errorf(
			"the step that must %s has no `id:`, so no attestation can bind its `subject-digest`"+
				" to the digest this run resolved.\nGive the signing step an `id:` and reference it"+
				" as `${{ steps.<id>.outputs.digest }}` in both attestation steps",
			sign.label)
	}

	evidence := evidenceMarkers(signStepID)

	for _, m := range evidence {
		at, err := locate(m)
		if err != nil {
			return err
		}

		if err := mustPrecede(publish, publishAt, m, at); err != nil {
			return err
		}

		if err := mustPrecede(m, at, release, releaseAt); err != nil {
			return err
		}
	}

	// Ordering is positional, so a step satisfies every comparison while carrying a condition that
	// never fires. GitHub then skips it WITHOUT failing the composite and runs the unconditional
	// release anyway, publishing without evidence the sequence claims is there.
	//
	// The test is not "may it have a condition" but "may it skip while the release still runs". A
	// step guarded by the SAME condition as the release stands or falls with it, so that stays legal;
	// anything else can vanish on its own.
	if err := mustNotSkipIndependently(steps, publish, publishAt, releaseAt); err != nil {
		return err
	}

	for _, m := range evidence {
		at, err := locate(m)
		if err != nil {
			return err
		}

		if err := mustNotSkipIndependently(steps, m, at, releaseAt); err != nil {
			return err
		}
	}

	for _, at := range signAt {
		if !assignsResolvedRef(steps[at].Run) {
			return fmt.Errorf(
				"the signing step must build its reference from the resolved digest (%s); "+
					"signing the mutable tag lets a concurrent deploy move it between resolve and sign",
				digestRef)
		}

		if !resolvesDigestFromPublishedTag(steps[at].Run) {
			return fmt.Errorf(
				"the signing step must resolve DIGEST from the tag it just published; a digest that "+
					"is written down rather than resolved lets cosign and both attestations cover an "+
					"older artifact in full while the newly pushed tag is released unsigned.\n"+
					"Required shape: DIGEST=$(<command> … %s …) — a command substitution whose LAST "+
					"statement is a single command taking %s as an argument, for example\n"+
					"  DIGEST=$(docker buildx imagetools inspect \"%s\" --format '{{.Manifest.Digest}}')\n"+
					"Any tool may resolve it. What is not accepted is a pipeline, an if/for/while/case, "+
					"a subshell or a && chain as that last statement: each of those can emit a value "+
					"the tag-reading command never produced, which is the exact substitution this "+
					"check exists to catch. Retry around the assignment instead "+
					"(for … do DIGEST=$(…) && break; done), which is supported",
				publishedTagRef, publishedTagRef, publishedTagRef)
		}

		if !signsTheResolvedDigest(steps[at]) {
			return fmt.Errorf(
				"the signing step resolves the digest into %s but does not pass %s to `cosign sign`; "+
					"the assignment is dead and the signature would cover whatever the mutable tag "+
					"resolves to at sign time",
				digestRef, signRefArg)
		}

		// Checked after the reference contract above, deliberately. Until `"${REF}"` is known to be
		// present, a stray expansion is more likely to be a wrong reference than a smuggled option,
		// and reporting it as an unrecognised OPTION would name the wrong defect.
		for _, cmd := range cosignSignCommands(steps[at]) {
			flag, unexpected := unexpectedSignFlag(cmd)
			if !unexpected {
				continue
			}

			return fmt.Errorf(
				"`cosign sign` carries the unrecognised option %q.\n"+
					"Options are allow-listed here because some of them make a successful signing "+
					"command publish nothing — `--upload=false` produces the signature and never "+
					"writes it to the registry, leaving the artifact unsigned while this step and "+
					"every ordering check pass.\n"+
					"Accepted today: --yes/-y, --recursive/-r. If this option is intended, add it to "+
					"signFlags in %s with a note on why it does not affect what reaches the registry",
				flag, "scripts/validate-publication-order/main.go")
		}
	}

	return nil
}

func run(paths []string, stderr io.Writer) error {
	if len(paths) == 0 {
		return errors.New("usage: validate-publication-order <action.yml>")
	}

	failed := false

	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			// Fail closed: an unreadable action is not a validated action.
			_, _ = fmt.Fprintf(stderr, "::error::%s: could not read: %v\n", path, err)
			failed = true

			continue
		}

		if err := validate(source); err != nil {
			_, _ = fmt.Fprintf(stderr, "::error::%s: %v\n", path, err)
			failed = true
		}
	}

	if failed {
		return errors.New("the publication ordering contract is not satisfied")
	}

	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}
