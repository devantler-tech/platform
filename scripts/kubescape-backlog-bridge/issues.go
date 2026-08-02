// Issue reconciliation: turning derived themes into create/update/close work.
package main

import (
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

// fingerprintPattern extracts the marker's hex identity.
//
// Anchored to a WHOLE LINE. Sanitising folds an embedded newline into a space,
// so a crafted component can still carry the marker text mid-line; unanchored,
// the identity would then be decided by which one appears first, and the entry's
// identity would depend on the order renderBody happens to emit its fields in.
// Anchoring makes only the line renderBody itself writes count.
var fingerprintPattern = regexp.MustCompile(`(?m)^<!-- ` + fingerprintMarker + `([0-9a-f]{16}) -->$`)

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
}

// fingerprint reads the entry's machine identity out of its body.
func (e backlogEntry) fingerprint() (string, error) {
	m := fingerprintPattern.FindStringSubmatch(e.Body)
	if m == nil {
		return "", fmt.Errorf("%w: issue #%d", errMissingFingerprint, e.Number)
	}

	return m[1], nil
}

// issueAction is one reconciliation step.
type issueAction struct {
	// Kind is "create", "update", "reopen" or "close".
	Kind string
	// Number is the existing issue, zero for a create.
	Number int
	Title  string
	Body   string
	// Reason explains a close in the issue's own timeline.
	Reason string
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
}

// empty reports whether the plan performs no writes at all. Unchanged cluster
// state MUST produce an empty plan — that is the anti-churn acceptance
// criterion, expressed as a property the caller can assert.
//
// Keyed on the actions alone: a withheld close is by definition not a write,
// and folding it in here would make a gated run look like it had work to do.
func (p plan) empty() bool { return len(p.Actions) == 0 }

// zeroWidthSpace breaks a token without removing information from it.
const zeroWidthSpace = "​"

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
		"`", "'",
		"\r", " ",
		"\n", " ",
	).Replace(s)

	return strings.TrimSpace(replaced)
}

// renderTitle and renderBody are the two fields a reconciler compares against
// the live issue to decide whether anything actually changed. Both are pure
// functions of the theme, with no timestamps, run ids, or ordering derived from
// map iteration — an unstable byte anywhere in either one turns every run into
// an update and reintroduces exactly the churn this design forbids.
func renderTitle(t theme) string { return sanitizeForIssue(t.Title()) }

// renderBody builds the issue body.
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
	fmt.Fprintf(&b, "**Severity:** %s\n", sanitizeForIssue(t.Severity))

	if t.Kind == string(surfaceCVE) {
		fmt.Fprintf(&b, "**Occurrences:** %d\n", t.Total)
	}

	fmt.Fprintf(&b, "\n**Affected components (%d):**\n\n", len(t.Components))

	for _, c := range t.Components {
		b.WriteString("- `" + sanitizeForIssue(c) + "`\n")
	}

	b.WriteString("\nThis issue is reconciled automatically: it is updated when the affected set " +
		"changes and closed when the finding is gone. Edit the title or body only if you are " +
		"willing to have it overwritten — the fingerprint comment above is what identifies it.\n")

	return b.String()
}

// closeComment explains why the reconciler closed an entry.
const closeComment = "> 🤖 Generated by the Agentic Engineer\n\n" +
	"Closing automatically: this finding is no longer present in the live Kubescape scan " +
	"surface this run examined in full. It will reopen on the same issue if it returns."

// planWrites reconciles derived themes against the issues already tracked.
//
// closeGone gates the destructive half and defaults OFF at every call site.
// Closing is only sound when the run examined EVERY object on a surface, and
// this command has no cluster inventory — it cannot tell a whole-cluster sweep
// from a single per-object GET. Absent that assertion a one-workload run would
// close every theme derived from the workloads it was not given, marking live
// findings resolved. Creates and updates stay safe under partial input, so they
// are never gated on it.
//
// examined scopes closing further: a run passed only -posture may not close CVE
// entries, whose surface it did not look at at all.
func planWrites(themes []theme, existing []backlogEntry, examined []surface, closeGone bool) (plan, error) {
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
		if !entry.Open {
			p.Actions = append(p.Actions,
				issueAction{Kind: "reopen", Number: entry.Number, Title: title, Body: body})

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

		p.Actions = append(p.Actions,
			issueAction{Kind: "update", Number: entry.Number, Title: title, Body: body})
	}

	var stale []backlogEntry

	for fp, e := range byFingerprint {
		if _, live := alive[fp]; live || !e.Open {
			continue
		}

		if !closeGone || !slices.Contains(examined, entrySurface(e)) {
			p.WithheldCloses++

			continue
		}

		stale = append(stale, e)
	}

	sort.Slice(stale, func(i, j int) bool { return stale[i].Number < stale[j].Number })

	for _, e := range stale {
		p.Actions = append(p.Actions,
			issueAction{Kind: "close", Number: e.Number, Reason: closeComment})
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
	for _, line := range strings.Split(e.Body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "**Surface:**")
		if !ok {
			continue
		}

		switch s := surface(strings.TrimSpace(rest)); s {
		case surfacePosture, surfaceCVE:
			return s
		default:
			return ""
		}
	}

	return ""
}
