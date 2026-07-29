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
	shadowed := declaredFunctions(file)

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

		// `set` changes how the shell treats a later failure, so it has to be tracked as state
		// rather than matched as a shape. Applied before the errexit test so `set -e` enables
		// itself, which is what makes an explicit re-enable after `set +e` work.
		if toggled, ok := errexitToggle(cmd.words); ok {
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
		// functions before PATH, so `cosign() { true; }` turns the required invocation into a no-op
		// that is otherwise indistinguishable from the real thing.
		if len(cmd.words) > 0 && shadowed[cmd.words[0]] {
			continue
		}

		out = append(out, cmd)
	}

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
func errexitToggle(words []string) (bool, bool) {
	if len(words) < 2 || words[0] != "set" {
		return false, false
	}

	state, found := false, false

	for i := 1; i < len(words); i++ {
		word := words[i]

		switch {
		case strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--") && strings.Contains(word, "e"):
			state, found = true, true
		case strings.HasPrefix(word, "+") && strings.Contains(word, "e"):
			state, found = false, true
		case (word == "-o" || word == "+o") && i+1 < len(words) && words[i+1] == "errexit":
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
	cmd := executedCommand{words: make([]string, 0, len(call.Args))}

	for _, arg := range call.Args {
		cmd.words = append(cmd.words, sourceOf(src, arg.Pos(), arg.End()))
	}

	return cmd
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
				consumesPublishedTag: valueResolvedFrom(assign.Value, publishedTagVar),
			})
		}
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		switch typed := node.(type) {
		case *syntax.CallExpr:
			collect(typed.Assigns)
		// `export`, `declare`, `local`, `readonly` and `typeset` assign too, and the parser models
		// them as a DeclClause rather than a CallExpr. Reading only CallExpr made
		// `declare -g DIGEST="sha256:…"` invisible, so a constant could silently override a correct
		// resolution — and an action that legitimately exported REF would have been rejected for
		// having no assignment at all.
		case *syntax.DeclClause:
			collect(typed.Args)
		}

		return true
	})

	return out
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
func valueResolvedFrom(word *syntax.Word, name string) bool {
	resolved := false

	syntax.Walk(word, func(node syntax.Node) bool {
		subst, isSubst := node.(*syntax.CmdSubst)
		if !isSubst {
			return !resolved
		}

		if len(subst.Stmts) > 0 && consumes(subst.Stmts[len(subst.Stmts)-1], name) {
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
// It recurses to any depth WITHIN that statement rather than demanding a bare call, because a
// pipeline, a retry loop, a subshell or a redirection around the resolver is ordinary correct code
// and the output still terminates in the digest. That is the deliberate split: the accepted SHAPE is
// narrowed to one statement, and the freedom inside it is kept — narrowing what is accepted, rather
// than enumerating what is forbidden, which is what left this guard bypassable for five rounds.
func consumes(stmt *syntax.Stmt, name string) bool {
	found := false

	syntax.Walk(stmt, func(node syntax.Node) bool {
		call, isCall := node.(*syntax.CallExpr)
		if !isCall {
			return !found
		}

		for _, arg := range call.Args {
			if expands(arg, name) {
				found = true

				return false
			}
		}

		return !found
	})

	return found
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

// ksailWorkload matches a step that runs `workload <op>` through that wrapper.
//
// The executable matters as much as the words: `ksail workload push` and `echo workload push` have
// identical shape, so a rule reading only the operation's words would accept a step that merely
// prints it.
func ksailWorkload(op string) func(step) bool {
	return func(s step) bool {
		for _, cmd := range parseExecuted(s.Run, errexitAtStart(s)) {
			if len(cmd.words) == 0 || !viaKsail(cmd.words[0]) {
				continue
			}

			for i := 1; i+1 < len(cmd.words); i++ {
				if cmd.words[i] == "workload" && cmd.words[i+1] == op {
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
		if len(cmd.words) >= 2 && cmd.words[0] == "cosign" && cmd.words[1] == "sign" {
			out = append(out, cmd)
		}
	}

	return out
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
	publish = marker{"publish the manifests (`workload push`)", ksailWorkload("push")}

	// sign is named separately because its step is inspected again below, for
	// the digest reference. Locating it by value rather than by matching its
	// label keeps that second lookup correct when the wording changes.
	sign = marker{"sign the published digest (`cosign sign`)", runsCosignSign}

	// evidence is what must exist before production may look. Order among
	// these is unconstrained.
	evidence = []marker{
		sign,
		{"attest the SBOM (`actions/attest`)", usesPrefix("actions/attest@")},
		{"attest build provenance (`actions/attest-build-provenance`)", usesPrefix("actions/attest-build-provenance@")},
	}

	// release is the step that advances what production can see.
	release = marker{"tell Flux to reconcile (`workload reconcile`)", ksailWorkload("reconcile")}
)

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

func validate(source []byte) error {
	var parsed action
	if err := yaml.Unmarshal(source, &parsed); err != nil {
		return fmt.Errorf("could not parse as YAML: %w", err)
	}

	steps := parsed.Runs.Steps
	if len(steps) == 0 {
		return errors.New("no runs.steps; this does not look like a composite action")
	}

	locate := func(m marker) ([]int, error) {
		at := indicesOf(steps, m)
		if len(at) == 0 {
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

	// sign is in evidence, so the loop above has already proved it resolves.
	signAt, err := locate(sign)
	if err != nil {
		return err
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
				"the signing step must resolve DIGEST from the tag it just published (a command "+
					"substitution reading %s); a digest that is written down rather than resolved "+
					"lets cosign and both attestations cover an older artifact in full while the "+
					"newly pushed tag is released unsigned",
				publishedTagRef)
		}

		if !signsTheResolvedDigest(steps[at]) {
			return fmt.Errorf(
				"the signing step resolves the digest into %s but does not pass %s to `cosign sign`; "+
					"the assignment is dead and the signature would cover whatever the mutable tag "+
					"resolves to at sign time",
				digestRef, signRefArg)
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
