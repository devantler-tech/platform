// Issue reconciliation: turning derived themes into create/update/close work.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// bridgeLabel marks every issue this command owns.
//
// Ownership is established by a LABEL rather than by searching bodies for the
// fingerprint marker. GitHub's free-text search ranks and stems, so it returns
// issues that merely mention a marker and can omit ones that carry it; a
// reconciler fed that list closes issues it does not own and re-files ones it
// does. A label listing is exact and complete, so the fingerprint is only ever
// parsed out of a body the label already proved is ours.
const bridgeLabel = "kubescape-bridge"

// githubIssueBodyLimit is the documented maximum length of an issue body.
const githubIssueBodyLimit = 65536

// githubIssueTitleLimit is the documented maximum length of an issue title.
const githubIssueTitleLimit = 256

// maxListedComponents bounds how many components a body enumerates.
//
// GitHub refuses an issue body over 65,536 characters. A component line is
// bounded to maxRenderedFieldRunes below, so 300 lines is at most ~56KB of list
// — comfortably inside the limit with the surrounding prose, while being far
// above any component count a real control produces (the live cluster carries
// ~2,215 posture summaries in total, across every control).
const maxListedComponents = 300

// maxRenderedFieldRunes bounds every scanner-derived field a body renders.
//
// maxListedComponents bounds how MANY components a body enumerates; nothing
// bounded how LONG each one — or the severity — may be, and both reach the body
// straight out of a scan document, so their length is not ours to assume. A
// single 70,000-character severity passes derivation and then pushes the
// assembled body past githubIssueBodyLimit on its own; GitHub refuses the create
// or edit, and applyPlan stops at the first failure, so one malformed scanner
// value blocks every remaining action in the run.
//
// Cut by RUNE, for the reason renderTitle gives: a byte cut can split a
// multi-byte rune into U+FFFD, which would differ from the live body every run
// and churn the issue forever.
//
// Bounding each FIELD rather than the assembled body is what keeps the cut
// prose-independent. A byte budget applied to the whole body would move as the
// surrounding wording is reworded, so the same theme would render differently
// across two versions and re-write every tracked issue — the exact churn the
// anti-churn guarantee forbids.
//
// 180 is the worst-case component length TestBodyStaysUnderGitHubsLimit already
// measured; with maxListedComponents that arithmetic leaves ~9K of headroom,
// which the same test now asserts against a real bound rather than against an
// assumed input length.
//
// Truncating cannot collide two themes into one issue: identity is the body's
// fingerprint, which Fingerprint derives from Kind and Key alone — never from a
// severity or a component.
const maxRenderedFieldRunes = 180

// fingerprintMarker is the machine-readable identity embedded in each body.
const fingerprintMarker = "kubescape-backlog-bridge:fingerprint="

// errAmbiguousEntry reports two tracked issues carrying one fingerprint.
//
// It is a hard error rather than a "pick the newest" heuristic. Two open issues
// for one theme is precisely the duplicate state this slice exists to prevent,
// so reaching it means an invariant already broke; guessing which one is
// authoritative would update one and silently strand the other, and the next
// run would make the same guess again. Refusing surfaces it once.
var errAmbiguousEntry = errors.New("two tracked issues carry the same fingerprint")

// errMissingFingerprint reports a labelled issue with no parsable marker.
//
// Also fail-closed: an issue wearing this command's label is claimed to be
// under its management, so an unreadable identity means either a human edited
// the marker out or a body format changed. Skipping it would leave the theme
// looking unfiled, and the next run would create a second issue for it.
var errMissingFingerprint = errors.New("tracked issue carries no readable fingerprint")

// errAmbiguousFingerprint reports an issue body carrying more than one marker.
//
// Fail-closed for the same reason entrySurface refuses a second "**Surface:**"
// line, and against the same edit. renderBody writes exactly one fingerprint, so
// a second is not something this command can produce; taking the first match
// would let an edited body place a forged marker ABOVE the genuine one and
// silently reassign the issue to another theme. A run would then close or
// overwrite that issue against the wrong finding and file a duplicate for its
// real one — so the identity is refused rather than guessed.
var errAmbiguousFingerprint = errors.New("tracked issue carries more than one fingerprint")

// fingerprintPattern extracts the marker's hex identity.
//
// Anchored to a WHOLE LINE. Sanitising folds an embedded newline into a space,
// so a crafted component can still carry the marker text mid-line; unanchored,
// the identity would then be decided by which one appears first, and the entry's
// identity would depend on the order renderBody happens to emit its fields in.
// Anchoring makes only the line renderBody itself writes count.
//
// The trailing \r? is load-bearing, not defensive noise. renderBody writes LF,
// but a body EDITED IN THE GITHUB WEB UI comes back from the API with CRLF, and
// Go's (?m)$ matches only before \n — so the \r would sit between `-->` and the
// anchor and the marker would not match. That is not a cosmetic miss: fingerprint
// failure is deliberately fail-closed, so planWrites returns errMissingFingerprint
// and EVERY subsequent run refuses, until someone hand-repairs the body. One
// maintainer edit through the web UI would otherwise wedge the bridge for good.
var fingerprintPattern = regexp.MustCompile(`(?m)^<!-- ` + fingerprintMarker + `([0-9a-f]{16}) -->\r?$`)

// backlogEntry is an issue this command already owns.
type backlogEntry struct {
	Number int
	Title  string
	Body   string
	// Open distinguishes an entry to update from one to reopen. Both are
	// matched, because a theme that returns after being resolved must land back
	// on its ORIGINAL issue: creating a fresh one for it would leave the
	// closed original as a permanent duplicate of a live finding.
	Open bool
	// Disposition is the close reason GitHub currently records, empty while the
	// entry is open.
	//
	// Carried because an absent finding's correct disposition is not fixed at
	// the moment of closing: a theme closed as remediated can later be covered
	// by a `ClusterSecurityException`, and one closed as accepted can later be
	// genuinely fixed. Both leave the issue absent from the scan, so neither
	// reopens — without the recorded value there is nothing to compare the
	// freshly derived disposition against, and the structured state stays
	// frozen at whatever the first close happened to decide.
	Disposition string
}

// fingerprint reads the entry's machine identity out of its body.
//
// Every marker line is counted, not just the first — see errAmbiguousFingerprint
// for why the second one is refused rather than ignored.
func (e backlogEntry) fingerprint() (string, error) {
	m := fingerprintPattern.FindAllStringSubmatch(e.Body, -1)
	if m == nil {
		return "", fmt.Errorf("%w: issue #%d", errMissingFingerprint, e.Number)
	}

	if len(m) > 1 {
		return "", fmt.Errorf("%w: issue #%d carries %d", errAmbiguousFingerprint, e.Number, len(m))
	}

	return m[0][1], nil
}

// issueAction is one reconciliation step.
type issueAction struct {
	// Kind is "create", "update", "reopen", "close" or "reclassify".
	//
	// "reclassify" corrects the structured disposition of an entry that is
	// ALREADY closed and stays closed. It is deliberately not a "close": the
	// issue's open/closed state does not change, and reporting it as a close
	// would tell an operator the run reconciled a stale entry when it only
	// re-labelled a settled one.
	Kind string
	// Number is the existing issue, zero for a create.
	Number int
	Title  string
	Body   string
	// Reason explains a close in the issue's own timeline.
	Reason string
	// Disposition is the GitHub close reason ("completed" or "not planned").
	//
	// Separate from Reason because the two say different things to different
	// readers: Reason is prose in the timeline, Disposition is the structured
	// state GitHub renders on the issue and filters by. A finding closed as
	// accepted is still live, so recording it as completed contradicts the very
	// comment posted alongside it.
	Disposition string
}

// plan is the full set of writes a run would perform, in a deterministic order.
type plan struct {
	Actions []issueAction
	// WithheldCloses counts tracked issues that have no matching finding and
	// were nonetheless left open — because -inputs-complete was absent, or
	// because this run never examined their surface.
	//
	// It is reported rather than merely counted. Without it a gated run prints
	// "no changes" while stale entries sit in the backlog, so an operator reads
	// a sound-looking all-clear as "the backlog matches the cluster". That is
	// this command's own core failure mode — a withheld action and a completed
	// one rendering identically — turned inward on its own output.
	WithheldCloses int
	// WithheldUpdates counts tracked issues whose rendered text differs from
	// this run's derivation but which were left untouched because the run did
	// not assert -inputs-complete.
	//
	// Disclosed for the same reason as WithheldCloses, and the drift it hides
	// runs the other way: the filed entry may name MORE components than this
	// run saw, so a silent skip leaves an operator believing the backlog was
	// reconciled when a real change — in either direction — is still pending.
	WithheldUpdates int
	// RacedCreates counts creates dropped by dropRacedCreates because another
	// invocation filed the same fingerprint between planning and applying.
	//
	// Disclosed for the same reason as the two counters above: the run performed
	// fewer writes than it planned, and a silent drop is indistinguishable from
	// a theme that never needed filing. It is also the only signal that write
	// mode is being run concurrently — which this command narrows the window on
	// but does not make safe — so a non-zero count is what tells an operator the
	// caller is missing its concurrency guard.
	RacedCreates int
	// UnverifiedDispositions counts already-closed entries whose disposition this
	// run could not check, because it was not authorised to conclude a finding is
	// absent — no -inputs-complete, or it never examined that entrys surface.
	//
	// Kept apart from WithheldCloses rather than folded into it: that counter's
	// operator note says the issues were "left OPEN", which is false of these and
	// would send someone looking for open work that does not exist. The
	// distinction matters most in the direction that reads as safe — an entry
	// stuck at Completed while an exception is doing the work looks remediated.
	UnverifiedDispositions int
}

// empty reports whether the plan performs no writes at all. Unchanged cluster
// state MUST produce an empty plan — that is the anti-churn acceptance
// criterion, expressed as a property the caller can assert.
//
// Keyed on the actions alone: a withheld close is by definition not a write,
// and folding it in here would make a gated run look like it had work to do.
func (p plan) empty() bool { return len(p.Actions) == 0 }

// zeroWidthSpace breaks a token without removing information from it.
//
// Written as an escape rather than the raw rune: U+200B is invisible in review
// and in a diff, so a raw literal can be silently dropped by an editor or a
// copy/paste without anything looking wrong. staticcheck flags the raw form
// (ST1018). The escape encodes the identical bytes, so rendered bodies are
// unchanged.
const zeroWidthSpace = "\u200b"

// sanitizeForIssue neutralises active GitHub syntax in a scanner-derived string
// before it is posted.
//
// Scan output is UNTRUSTED input. Severity names reach a theme after only a
// blank check, and CVE keys come from the summary object's own map keys, so an
// arbitrary string can reach a title or body — which this command posts from an
// authenticated account into a public repository. Two things then go wrong, and
// neither is cosmetic:
//
//   - An "@" makes a live mention. It would notify real people, and a body
//     containing a review-bot trigger would fire that bot from our own comment.
//     No Markdown construct hides a mention from a bot — not a code span, a
//     fence, a blockquote or an HTML comment — because bots parse the raw text,
//     so the token itself has to be broken.
//
//   - A "#" makes a live issue reference. GitHub turns "#123" into a link AND
//     posts a cross-reference on that issue, from this command's authenticated
//     account — so scan data carrying one generates notifications and timeline
//     noise on an unrelated issue that nobody chose to involve. Unlike a
//     mention, a code span DOES suppress this, and components are rendered
//     inside one; but Severity is not, and binding a value's safety to where it
//     happens to be interpolated is the mistake the newline case above already
//     rejects. Break the token instead, so every field is safe wherever it lands.
//
//   - A "<" opens live HTML. GitHub renders inline HTML in Markdown, so a
//     severity or CVE bucket carrying "<details><summary>…" emits real structure
//     into the body — collapsing or hiding the affected-component list that is
//     the whole point of the issue. Severity is interpolated outside a code
//     span, so nothing suppresses it there.
//
//     Broken with a zero-width space rather than escaped to "&lt;": components
//     render INSIDE a code span, where entities are not decoded, so escaping
//     would corrupt a legitimate component name into a visible "&lt;". Breaking
//     the token reads correctly in both places, which is the property this
//     function is for. A ">" needs no handling — without a "<" it opens nothing,
//     and newlines are already stripped, so a value can never start a line and
//     forge a blockquote.
//
//   - A "[" opens a Markdown link, and "![" an IMAGE. The image is the sharper
//     of the two: rendering it makes the issue page fetch a scanner-controlled
//     URL, turning a public security issue into a tracking beacon, and a wide
//     image can push the affected-component list out of view entirely. Breaking
//     "[" defeats both, because "![" is only an image when the bracket follows
//     the bang immediately.
//
//   - A "*" opens emphasis, and GitHub applies it INTRA-WORD, so a lone "*" in a
//     scanner value can italicise the rest of the line and obscure the finding.
//
//     "_" is deliberately NOT broken: GFM disables intra-word "_" emphasis, so
//     it cannot open anything mid-value — and underscores are ubiquitous in the
//     control IDs this field carries (CKV_K8S_40), where breaking them would
//     corrupt every copy/paste for no safety gain.
//
//   - A NEWLINE lets the string forge body structure — a second
//     "**Surface:**" line, or a second fingerprint marker. Today both readers
//     take the FIRST match and the genuine line is rendered above the component
//     list, so a forged one loses; that was measured, not assumed. But the
//     safety of a value must not rest on where it happens to be interpolated,
//     because moving one field would silently make a crafted workload name able
//     to retarget which entries a run closes. Stripping the newline makes the
//     value safe wherever it lands.
//
// Backticks are removed for the same reason at a smaller scale: components are
// rendered inside a code span, and a backtick would end it early and leave the
// rest of the value as live markup.
//
// The replacement is deterministic, so it cannot perturb the anti-churn
// guarantee: the same input always renders the same bytes.
func sanitizeForIssue(s string) string {
	replaced := strings.NewReplacer(
		"@", "@"+zeroWidthSpace,
		"#", "#"+zeroWidthSpace,
		"<", "<"+zeroWidthSpace,
		"[", "["+zeroWidthSpace,
		"*", "*"+zeroWidthSpace,
		"`", "'",
		"\r", " ",
		"\n", " ",
	).Replace(s)

	return strings.TrimSpace(replaced)
}

// boundField sanitizes a scanner-derived value and bounds its rendered length.
//
// Every field renderBody writes from scan data goes through this rather than
// through sanitizeForIssue directly; see maxRenderedFieldRunes for why an
// unbounded one can wedge the whole run, and why the cut is per field and by
// rune.
func boundField(s string) string {
	sanitized := sanitizeForIssue(s)

	runes := []rune(sanitized)
	if len(runes) <= maxRenderedFieldRunes {
		return sanitized
	}

	// A prefix cut alone is not enough here, and the failure is silent. Two
	// distinct components sharing a long prefix — `ns/Deployment/<200 chars>-a`
	// and `…-b` — would render as the SAME bullet, so the body reports the right
	// component COUNT while showing duplicate, non-identifying entries. Naming
	// the affected resources is the entire purpose of that list.
	//
	// A short digest of the FULL value restores distinctness: it is derived
	// before truncation, so it distinguishes values that differ anywhere,
	// including past the cut. It is deterministic, so an unchanged component
	// renders identically across runs and the anti-churn guarantee holds.
	//
	// Kept inside the budget, like the ellipsis, so the bound this function
	// promises is still exact.
	const ellipsis = "…"

	digest := sha256.Sum256([]byte(sanitized))
	suffix := ellipsis + hex.EncodeToString(digest[:])[:8]
	keep := maxRenderedFieldRunes - len([]rune(suffix))

	return string(runes[:keep]) + suffix
}

// renderTitle bounds the title for the same reason renderBody bounds the list:
// GitHub refuses a title over githubIssueTitleLimit, applyPlan stops at the
// first failure, so ONE over-long scanner-derived key would block every
// remaining action in the run. t.Key reaches the title from a CVE summary's own
// map keys or a control map key, so its length is not ours to assume.
//
// Cut by RUNE, not by byte: a byte cut can split a multi-byte rune and emit
// U+FFFD, which would differ from the live title every run and churn the issue
// forever — the exact failure the anti-churn guarantee forbids. Rune-truncation
// of identical input yields identical bytes, so a bounded title is as stable as
// an unbounded one.
//
// Truncating cannot collide two themes into one issue: identity is the body's
// fingerprint, never the title.
//
// It CAN, however, collide them for a human reader, which is why the digest
// below exists. The title is the only place t.Key is rendered — renderBody does
// not carry it — so a prefix-only cut of two keys sharing a long prefix
// produces two issues with identical visible titles, and when their severity
// and components also match, identical visible bodies. Only the hidden
// fingerprint would distinguish them, and nobody reads that. A digest of the
// full title, taken before the cut, restores distinctness for exactly the same
// reason boundField takes one.
func renderTitle(t theme) string {
	title := sanitizeForIssue(t.Title())

	runes := []rune(title)
	if len(runes) <= githubIssueTitleLimit {
		return title
	}

	// The suffix is inside the budget, not added to it.
	const ellipsis = "…"

	digest := sha256.Sum256([]byte(title))
	suffix := ellipsis + hex.EncodeToString(digest[:])[:8]

	return string(runes[:githubIssueTitleLimit-len([]rune(suffix))]) + suffix
}

// renderBody builds the issue body.
//
// renderBody and renderTitle are the two fields a reconciler compares against
// the live issue to decide whether anything actually changed. Both are pure
// functions of the theme, with no timestamps, run ids, or ordering derived from
// map iteration — an unstable byte anywhere in either one turns every run into
// an update and reintroduces exactly the churn this design forbids.
//
// It carries the SANITIZED MINIMUM: the control or CVE class, the affected
// components as namespace/kind/name, and the counts. Node names, pod IPs, image
// digests, UIDs and wlid internals are already excluded upstream by
// component.String, and nothing here reintroduces them — this body is a public
// artifact, and detailed reachability evidence belongs in the private operator
// notes instead.
func renderBody(t theme) string {
	var b strings.Builder

	b.WriteString("> 🤖 Generated by the Agentic Engineer\n\n")
	b.WriteString("<!-- " + fingerprintMarker + t.Fingerprint() + " -->\n\n")
	b.WriteString("Filed automatically from live Kubescape findings by ")
	b.WriteString("`scripts/kubescape-backlog-bridge`. Part of #2854.\n\n")

	// t.Kind is this package's own constant and is written raw, because
	// entrySurface parses this exact line back and must match surfacePosture or
	// surfaceCVE literally. Every other field is scanner-derived and sanitized.
	fmt.Fprintf(&b, "**Surface:** %s\n", t.Kind)
	fmt.Fprintf(&b, "**Severity:** %s\n", boundField(t.Severity))

	if t.Kind == string(surfaceCVE) {
		fmt.Fprintf(&b, "**Occurrences:** %d\n", t.Total)
	}

	fmt.Fprintf(&b, "\n**Affected components (%d):**\n\n", len(t.Components))

	// The COUNT above is always the true one; only the enumeration below is
	// bounded. A control failing on most of the cluster's objects renders a list
	// that can pass GitHub's 65,536-character body limit, and the create or edit
	// call is then refused — which, because applyPlan stops at the first failure,
	// blocks every remaining action in the run rather than just this one issue.
	//
	// Bounded by component COUNT rather than by measuring the assembled string:
	// a byte-budget cut would move as the surrounding prose is reworded, so the
	// same theme could render differently across two versions and churn every
	// tracked issue. A fixed count renders identical bytes for identical input,
	// which is what the anti-churn guarantee requires.
	listed := t.Components
	if len(listed) > maxListedComponents {
		listed = listed[:maxListedComponents]
	}

	for _, c := range listed {
		b.WriteString("- `" + boundField(c) + "`\n")
	}

	if omitted := len(t.Components) - len(listed); omitted > 0 {
		fmt.Fprintf(&b, "- …and %d more (%d affected in total; the full set is in the scan output)\n",
			omitted, len(t.Components))
	}

	b.WriteString("\nThis issue is reconciled automatically: it is updated when the affected set " +
		"changes and closed when the finding is gone. Edit the title or body only if you are " +
		"willing to have it overwritten — the fingerprint comment above is what identifies it.\n")

	return b.String()
}

// closeComment explains why the reconciler closed an entry that the scan no
// longer reports at all.
const closeComment = "> 🤖 Generated by the Agentic Engineer\n\n" +
	"Closing automatically: this finding is no longer present in the live Kubescape scan " +
	"surface this run examined in full. It will reopen on the same issue if it returns."

// GitHub's two close dispositions. A finding that is gone was completed; one
// covered by an exception is still live and was decided against, which is what
// "not planned" records and what a board filter can then tell apart.
const (
	dispositionCompleted  = "completed"
	dispositionNotPlanned = "not planned"
)

// normalizeDisposition maps what `gh issue list --json stateReason` reports onto
// the vocabulary above.
//
// THREE spellings of one concept are in play and no two of them match: the list
// surface reports COMPLETED / NOT_PLANNED, `gh issue close --reason` takes
// "completed" / "not planned" with a SPACE, and REST state_reason takes
// completed / not_planned with an UNDERSCORE. Comparing a raw list value against
// dispositionNotPlanned is therefore never equal, so an already-closed accepted
// entry would read as mis-dispositioned on EVERY run and be rewritten forever —
// a silent write loop against the API, not a visible failure.
//
// An unrecognised value returns "" rather than guessing a default. "" never
// equals a derived disposition, so an entry closed as `duplicate` (a reason this
// command never writes but a human can) is corrected once to what the scan
// actually supports, instead of being silently treated as already-correct.
func normalizeDisposition(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "COMPLETED":
		return dispositionCompleted
	case "NOT_PLANNED", "NOT PLANNED":
		return dispositionNotPlanned
	default:
		return ""
	}
}

// reclassifyComment explains a disposition corrected on an entry that was
// already closed and stays closed.
//
// Deliberately NOT closeComment or acceptedComment: both of those announce a
// close that is happening now, and posting one under an issue closed weeks ago
// would misdate the event in the only record a reader has. This wording says
// what changed and why the issue is not reopening.
func reclassifyComment(disposition string) string {
	if disposition == dispositionNotPlanned {
		return "> 🤖 Generated by the Agentic Engineer\n\n" +
			"Correcting this issue's close reason to **not planned**. It was recorded as " +
			"remediated, but every occurrence of the finding is now covered by a declared " +
			"`ClusterSecurityException` — so it is accepted rather than fixed, and the finding " +
			"is still present in the live scan. The issue stays closed; only the classification " +
			"was wrong. It will reopen on this same issue if the exception is withdrawn."
	}

	return "> 🤖 Generated by the Agentic Engineer\n\n" +
		"Correcting this issue's close reason to **completed**. It was recorded as accepted " +
		"under a `ClusterSecurityException`, but the finding is no longer present in the live " +
		"Kubescape scan surface this run examined in full — it was remediated, not merely " +
		"suppressed. The issue stays closed; only the classification was wrong."
}

// acceptedComment explains why the reconciler closed an entry whose every
// occurrence a declared ClusterSecurityException covers.
//
// It is a SEPARATE wording, not a nicety. Both paths reach the stale loop by the
// same route — the theme is absent from this run's derivation — but they are
// opposite facts about the cluster: one says the finding is gone, the other says
// it is still there and has been accepted. Closing an accepted finding as "no
// longer present" writes the wrong one into the issue's permanent timeline,
// precisely when a reader most needs to know an exception is load-bearing.
const acceptedComment = "> 🤖 Generated by the Agentic Engineer\n\n" +
	"Closing automatically: every occurrence of this finding is covered by a declared " +
	"`ClusterSecurityException`, so it is accepted rather than remediated — the finding is " +
	"still present in the live scan. It will reopen on the same issue if the exception is " +
	"narrowed or removed."

// planWrites reconciles derived themes against the issues already tracked.
//
// inputsComplete is the caller's assertion that this run examined EVERY object
// on the surfaces it names, and it defaults OFF at every call site. This command
// has no cluster inventory — it cannot tell a whole-cluster sweep from a single
// per-object GET — so two of the three write shapes are unsound without it:
//
//   - CLOSE, because a one-workload run would otherwise close every theme
//     derived from the workloads it was not given, marking live findings
//     resolved.
//   - UPDATE, because a theme rendered from a subset lists only that subset's
//     components. Overwriting a tracked entry with it drops the components this
//     run did not look at and can lower the recorded occurrence count or
//     severity while those findings are still live — a silent downgrade that
//     reads exactly like remediation.
//
// CREATE stays ungated: a theme nobody tracks yet is new information, and an
// under-scoped new entry is corrected by the first complete run. A REOPEN is
// likewise never withheld — leaving a live finding closed is the worse error —
// but under partial input it reopens carrying the entry's ALREADY-FILED title
// and body rather than the subset render, so correcting the state cannot cost
// the scope the last complete run recorded.
//
// examined scopes closing further: a run passed only -posture may not close CVE
// entries, whose surface it did not look at at all.
//
// accepted holds the fingerprints of themes every occurrence of which a declared
// exception suppressed. They are absent from themes for a reason opposite to the
// one the close path assumes, so they close with acceptedComment instead.
func planWrites(
	themes []theme,
	existing []backlogEntry,
	examined []surface,
	inputsComplete bool,
	accepted map[string]struct{},
) (plan, error) {
	byFingerprint := map[string]backlogEntry{}

	for _, e := range existing {
		fp, err := e.fingerprint()
		if err != nil {
			return plan{}, err
		}

		if prev, dup := byFingerprint[fp]; dup {
			return plan{}, fmt.Errorf("%w: #%d and #%d both claim %s",
				errAmbiguousEntry, prev.Number, e.Number, fp)
		}

		byFingerprint[fp] = e
	}

	// Sort themes so the action order is stable run over run. Themes arrive
	// grouped by surface in derivation order, which is stable today, but an
	// action list whose ORDER depends on that is a latent diff for any future
	// caller that merges two derivations.
	sorted := slices.Clone(themes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}

		return sorted[i].Key < sorted[j].Key
	})

	var (
		p     plan
		alive = map[string]struct{}{}
	)

	for _, t := range sorted {
		fp := t.Fingerprint()
		alive[fp] = struct{}{}

		title, body := renderTitle(t), renderBody(t)

		entry, tracked := byFingerprint[fp]
		if !tracked {
			p.Actions = append(p.Actions, issueAction{Kind: "create", Title: title, Body: body})

			continue
		}

		// A closed entry whose theme is present again is REOPENED, never
		// re-created: the original carries the discussion and the history, and
		// a fresh issue for a recurring finding is a duplicate by construction.
		//
		// Under partial input the state change still happens — a live finding
		// left closed is the worse error — but it carries the entry's existing
		// text, so reopening cannot narrow the component set the last complete
		// run filed.
		if !entry.Open {
			reopenTitle, reopenBody := title, body
			if !inputsComplete {
				reopenTitle, reopenBody = entry.Title, entry.Body
			}

			p.Actions = append(p.Actions,
				issueAction{Kind: "reopen", Number: entry.Number, Title: reopenTitle, Body: reopenBody})

			continue
		}

		// The anti-churn guarantee, and the reason it lives here rather than in
		// the client: unchanged state renders byte-identical text, so there is
		// nothing to write. Comparing the RENDERED fields — not a change flag
		// threaded down from derivation — means any future field that happens
		// to be unstable shows up as a failing zero-write test rather than as
		// a quiet stream of daily updates on a healthy cluster.
		if entry.Title == title && entry.Body == body {
			continue
		}

		// Ordered AFTER the byte comparison on purpose: a partial run over an
		// unchanged theme has nothing to write either way, and counting it as
		// withheld would report phantom pending work on every subset run.
		if !inputsComplete {
			p.WithheldUpdates++

			continue
		}

		p.Actions = append(p.Actions,
			issueAction{Kind: "update", Number: entry.Number, Title: title, Body: body})
	}

	// staleClose pairs each entry with the wording its close will carry. The
	// reason is decided HERE, where the map key is already the fingerprint, so
	// the entry is never re-parsed for an identity this loop is holding.
	type staleClose struct {
		entry       backlogEntry
		reason      string
		disposition string
	}

	var stale, reclassify []staleClose

	for fp, e := range byFingerprint {
		// Only liveness short-circuits here. An ALREADY-CLOSED entry used to be
		// skipped alongside it, which froze its disposition at whatever the
		// first close decided: a theme closed as remediated and later covered by
		// an exception is absent from the scan either way, so nothing reopens it
		// and nothing revisits it. The audit record then says Completed for a
		// finding that is still present in the cluster — the fail-open direction
		// for a security backlog, and invisible because the issue looks settled.
		if _, live := alive[fp]; live {
			continue
		}

		if !inputsComplete || !slices.Contains(examined, entrySurface(e)) {
			// Counted only for entries that are still OPEN. A closed entry is
			// not a tracked issue "left open", and folding it into this counter
			// would make the operator note report a backlog state that does not
			// exist.
			//
			// The closed-entry count is UNVERIFIED, not disagreeing: this branch
			// is reached before any disposition is derived, and it cannot be
			// derived here — whether the finding is absent is exactly what an
			// unexamined surface does not establish. A CVE-only run would
			// otherwise report every correctly-classified closed posture entry as
			// mismatched, turning a clean surface-specific run into one that looks
			// to have pending work. The note wording says unverified for that
			// reason.
			if e.Open {
				p.WithheldCloses++
			} else {
				p.UnverifiedDispositions++
			}

			continue
		}

		reason, disposition := closeComment, dispositionCompleted
		if _, isAccepted := accepted[fp]; isAccepted {
			reason, disposition = acceptedComment, dispositionNotPlanned
		}

		if !e.Open {
			// Idempotence is the whole safety property of this branch: it runs on
			// every absent closed entry, every run, forever. Acting only on a
			// genuine disagreement is what keeps a settled backlog silent.
			if e.Disposition == disposition {
				continue
			}

			reclassify = append(reclassify,
				staleClose{entry: e, reason: reclassifyComment(disposition), disposition: disposition})

			continue
		}

		stale = append(stale, staleClose{entry: e, reason: reason, disposition: disposition})
	}

	sort.Slice(stale, func(i, j int) bool { return stale[i].entry.Number < stale[j].entry.Number })

	for _, s := range stale {
		p.Actions = append(p.Actions,
			issueAction{
				Kind:        "close",
				Number:      s.entry.Number,
				Reason:      s.reason,
				Disposition: s.disposition,
			})
	}

	// Emitted after the closes and independently sorted, so the action order
	// stays deterministic for a given input regardless of map iteration order.
	sort.Slice(reclassify, func(i, j int) bool {
		return reclassify[i].entry.Number < reclassify[j].entry.Number
	})

	for _, s := range reclassify {
		p.Actions = append(p.Actions,
			issueAction{
				Kind:        "reclassify",
				Number:      s.entry.Number,
				Reason:      s.reason,
				Disposition: s.disposition,
			})
	}

	return p, nil
}

// entrySurface recovers which scan surface a tracked issue belongs to.
//
// It reads the rendered "**Surface:** x" line rather than re-deriving it from
// the title, because the title is prose that may be reworded while the labelled
// field is written by renderBody and is what a reader is told identifies it.
// An entry whose surface cannot be read returns an empty surface, which matches
// no examined surface and is therefore never closed — fail-closed, since the
// cost of failing to close is a stale issue and the cost of closing wrongly is
// a live finding marked resolved.
func entrySurface(e backlogEntry) surface {
	var (
		found surface
		seen  int
	)

	// Every "**Surface:**" line is examined, not just the first. renderBody
	// writes exactly one, so a second is not something this command can produce
	// — and taking the first match would let an edited body put a forged line
	// ABOVE the genuine one and silently retarget which surface the entry claims
	// to belong to. That decides whether a run may close or reclassify it, so
	// the ambiguity is resolved fail-closed: an unrecognised value, a duplicate,
	// or none at all all yield "", which reads as "surface unknown" and makes
	// the lifecycle gates withhold rather than authorise.
	for _, line := range strings.Split(e.Body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "**Surface:**")
		if !ok {
			continue
		}

		seen++
		if seen > 1 {
			return ""
		}

		switch s := surface(strings.TrimSpace(rest)); s {
		case surfacePosture, surfaceCVE:
			found = s
		default:
			return ""
		}
	}

	return found
}
