// GitHub-backed issue client and the write-mode driver.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// errWriteFailed reports a reconciliation step the forge refused.
var errWriteFailed = errors.New("issue write failed")

// issueStore is the forge surface the reconciler needs.
//
// It exists so the planner and the driver are testable without a network or a
// live repository. A fake implementation records the calls, which is the only
// way to assert the property that actually matters — that an unchanged cluster
// produces NO calls at all. A test that merely inspected a returned plan could
// not catch a driver that wrote anyway.
type issueStore interface {
	// list returns every issue this command owns, open and closed.
	list() ([]backlogEntry, error)
	create(title, body string) (int, error)
	update(number int, title, body string) error
	reopen(number int, title, body string) error
	// close carries the GitHub disposition as well as the comment: an accepted
	// finding is still live, so recording it as Completed contradicts the comment
	// the same call posts.
	close(number int, comment, reason string) error
	// reclassify corrects the disposition of an entry that is already closed and
	// stays closed, posting the comment that explains the new classification.
	//
	// Separate from close because `gh issue close` refuses an already-closed
	// issue: routing this through close would fail the whole run at the first
	// re-classification, and applyPlan stops at the first failure.
	reclassify(number int, comment, reason string) error
}

// applyPlan executes a plan against a store, narrating each step.
//
// It stops at the first failure rather than continuing. A partially applied
// plan is recoverable — the next run re-derives the same themes and reconciles
// whatever is left — but a run that pressed on through an auth or rate-limit
// error would emit a long list of identical failures and bury the first cause.
func applyPlan(p plan, store issueStore, out io.Writer) error {
	// Announced BEFORE the actions, and on the no-op path too: a run that
	// declined to close stale entries has not reconciled the backlog, and
	// saying only "no changes" would report that as a clean result.
	if p.WithheldCloses > 0 {
		if _, err := fmt.Fprintf(out,
			"note: %d tracked issue(s) have no matching finding but were left OPEN — closing needs "+
				"-inputs-complete and a run that examined their surface, so the backlog is not "+
				"fully reconciled\n", p.WithheldCloses); err != nil {
			return err
		}
	}

	// Same reasoning, other direction: the filed entry disagrees with what this
	// run derived, and the run declined to overwrite it because a subset render
	// would drop the components it never looked at.
	if p.WithheldUpdates > 0 {
		if _, err := fmt.Fprintf(out,
			"note: %d tracked issue(s) differ from this run's findings but were left UNCHANGED — "+
				"updating needs -inputs-complete, since a subset render would drop components this "+
				"run did not examine\n", p.WithheldUpdates); err != nil {
			return err
		}
	}

	// A closed entry whose recorded disposition this run could not CHECK — the
	// counter is incremented in the gate branch, before any disposition is
	// derived, so it records "not verified", never "found to disagree".
	// Announced separately from WithheldCloses because these issues are closed,
	// not open: an operator told they were "left OPEN" would go looking for
	// backlog work that is not there.
	if p.UnverifiedDispositions > 0 {
		if _, err := fmt.Fprintf(out,
			"note: %d closed issue(s) had their disposition left UNVERIFIED — checking one needs "+
				"-inputs-complete and a run that examined their surface, so the structured record "+
				"may still say a finding was remediated when an exception is what is covering it\n", p.UnverifiedDispositions); err != nil {
			return err
		}
	}

	// A create this run planned and then dropped because a concurrent
	// invocation had already filed it. Reported before the actions for the same
	// reason as the withheld counts: the run wrote less than it planned, and the
	// operator needs to know their caller is running write mode concurrently.
	if p.RacedCreates > 0 {
		if _, err := fmt.Fprintf(out,
			"note: %d planned create(s) were DROPPED — another invocation filed the same "+
				"fingerprint between this run's plan and its apply. Serialize write mode "+
				"(a concurrency group with cancel-in-progress disabled); the next run "+
				"reconciles the issues it filed\n", p.RacedCreates); err != nil {
			return err
		}
	}

	if p.empty() {
		// A run that withheld something did NOT find the backlog already matching,
		// and saying so two lines under a note that a tracked issue differs is a
		// self-contradicting summary an operator can reasonably read as
		// synchronized. The withheld notes above say what is pending; this line
		// only claims the writes it actually decided against making.
		line := "no changes: every derived theme already matches its tracked issue"
		if p.WithheldCloses > 0 || p.WithheldUpdates > 0 || p.RacedCreates > 0 ||
			p.UnverifiedDispositions > 0 {
			line = "no writes performed: this run planned none beyond the withheld or dropped " +
				"action(s) noted above"
		}

		_, err := fmt.Fprintln(out, line)

		return err
	}

	for _, a := range p.Actions {
		var err error

		switch a.Kind {
		case "create":
			var number int

			number, err = store.create(a.Title, a.Body)
			if err == nil {
				_, err = fmt.Fprintf(out, "created #%d\t%s\n", number, a.Title)
			}
		case "update":
			if err = store.update(a.Number, a.Title, a.Body); err == nil {
				_, err = fmt.Fprintf(out, "updated #%d\t%s\n", a.Number, a.Title)
			}
		case "reopen":
			if err = store.reopen(a.Number, a.Title, a.Body); err == nil {
				_, err = fmt.Fprintf(out, "reopened #%d\t%s\n", a.Number, a.Title)
			}
		case "close":
			if err = store.close(a.Number, a.Reason, a.Disposition); err == nil {
				_, err = fmt.Fprintf(out, "closed #%d\n", a.Number)
			}
		case "reclassify":
			// Narrated with the resulting disposition, not just the number: the
			// issue was closed before this run and is closed after it, so the
			// only thing that changed is the word an operator would otherwise
			// have to open GitHub to see.
			if err = store.reclassify(a.Number, a.Reason, a.Disposition); err == nil {
				_, err = fmt.Fprintf(out, "reclassified #%d\tas %s\n", a.Number, a.Disposition)
			}
		default:
			err = fmt.Errorf("%w: unknown action %q", errWriteFailed, a.Kind)
		}

		if err != nil {
			return fmt.Errorf("%w: %s: %w", errWriteFailed, a.Kind, err)
		}
	}

	return nil
}

// ghStore drives the `gh` CLI against one repository.
type ghStore struct {
	repo string
	// labelEnsured records that the ownership label has been provisioned for this
	// store, so the check costs one call per run rather than one per issue.
	labelEnsured bool
	// run is the command runner, replaceable in tests.
	run func(args ...string) ([]byte, error)
}

func newGHStore(repo string) *ghStore {
	return &ghStore{repo: repo, run: func(args ...string) ([]byte, error) {
		cmd := exec.Command("gh", args...)

		outputBytes, err := cmd.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
				return nil, fmt.Errorf("gh %s: %w: %s",
					strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
			}

			return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
		}

		return outputBytes, nil
	}}
}

// ghIssue mirrors the `gh issue list --json` shape this command reads.
type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	// StateReason is GitHub's structured close reason, empty while the issue is
	// open. Reported here as COMPLETED / NOT_PLANNED — a THIRD spelling of the
	// same concept, see normalizeDisposition.
	StateReason string `json:"stateReason"`
}

// listLimit bounds one `gh issue list` page.
//
// `gh issue list` defaults to THIRTY, and a truncated listing is the worst
// possible input to a reconciler: every theme whose issue fell off the end
// looks unfiled, so the run re-creates it as a duplicate. The limit is set far
// above any plausible theme count and the result is checked against it, so
// truncation becomes a hard error rather than a silent duplicate storm.
const listLimit = 500

func (g *ghStore) list() ([]backlogEntry, error) {
	raw, err := g.run("issue", "list",
		"--repo", g.repo,
		"--label", bridgeLabel,
		"--state", "all",
		"--limit", fmt.Sprint(listLimit),
		"--json", "number,title,body,state,stateReason")
	if err != nil {
		return nil, err
	}

	var issues []ghIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("decoding gh issue list: %w", err)
	}

	// The ceiling counts CLOSED issues too: --state all is deliberate (a
	// returning theme must land back on its original issue), and a closed issue
	// keeps the label. So this is a LIFETIME budget, not a concurrent one, and
	// it is reached by ordinary successful use rather than by anything going
	// wrong. Naming the recovery step here is the difference between a stop an
	// operator can clear and one they have to reverse-engineer.
	if len(issues) >= listLimit {
		return nil, fmt.Errorf("%w: %d tracked issues hit the --limit %d ceiling, so the listing "+
			"may be truncated and every omitted theme would be re-filed as a duplicate. "+
			"This ceiling counts closed issues too, so it is a lifetime total. To clear it, "+
			"remove the %q label from issues that are closed and no longer need tracking "+
			"(they are re-filed only if the finding returns), or raise listLimit",
			errWriteFailed, len(issues), listLimit, bridgeLabel)
	}

	entries := make([]backlogEntry, 0, len(issues))
	for _, i := range issues {
		entries = append(entries, backlogEntry{
			Number:      i.Number,
			Title:       i.Title,
			Body:        i.Body,
			Open:        strings.EqualFold(i.State, "open"),
			Disposition: normalizeDisposition(i.StateReason),
		})
	}

	return entries, nil
}

// createLabels is the single source of truth for the labels a new issue wears.
//
// ensureLabel provisions exactly this set and create applies exactly this set,
// from the same slice, so the two cannot drift. Provisioning a subset is not a
// cosmetic mismatch: an unprovisioned label fails the create outright (see
// ensureLabel), and applyPlan stops at the first failure, so the run files
// nothing at all.
var createLabels = []struct {
	name        string
	description string
	// color pins an owned label's appearance so --force is genuinely
	// idempotent. `gh label create` documents its colour default as RANDOM, so
	// forcing without one re-rolls the colour on every run that has an issue to
	// create — churning the label's appearance and its audit timeline while the
	// surrounding code claims the provisioning is a no-op. Empty for a label
	// this command does not own, which is never forced and whose colour is the
	// repository's to choose.
	color string
	// owned marks a label this command created and may therefore keep
	// up to date. A label it merely APPLIES belongs to the repository, and
	// its appearance is not ours to change — see ensureLabel.
	owned bool
}{
	{bridgeLabel, "Filed automatically from live Kubescape findings", "0e8a16", true},
	{"security", "", "", false},
}

// ensureLabel provisions every label a create applies.
//
// `gh issue create --label` ASSOCIATES an existing label; it does not create
// one. On a repository that has never run this command the label does not
// exist — verified against devantler-tech/platform, which carries no
// kubescape-bridge label and no setup for it outside this package — so the very
// first untracked theme would fail and, because applyPlan stops at the first
// failure, no backlog issue would be filed at all. The failure would appear only
// on a first run against a fresh repository, which is exactly when nobody is
// watching for it.
//
// EVERY label in createLabels is provisioned, not just the ownership one.
// devantler-tech/platform happens to carry `security` already, so on today's
// target this is latent rather than live — but -repo is a flag, and on any
// repository lacking it the first create would fail and file nothing.
//
// The two kinds are provisioned DIFFERENTLY, and the difference is not
// cosmetic. `gh label create --force` updates an existing label's colour and
// description, and when no colour is passed gh picks a RANDOM one — so forcing
// a label this command does not own would silently recolour and re-describe a
// label the repository already maintains for its own purposes, on every fresh
// run. devantler-tech/platform's `security` is a deliberate red (#b60205) with
// no description; that is the repository's choice, not this command's.
//
//   - owned (bridgeLabel): --force, with our description AND a pinned colour.
//     It exists only because this command creates it, so keeping it current is
//     correct — but the colour has to be pinned for the call to be idempotent
//     at all, since gh's colour default is random.
//   - not owned (security): a plain create, and an error is DELIBERATELY
//     ignored. The only property that matters is that the label exists
//     afterwards; if it already did, "already exists" is success spelled as a
//     failure. A genuine inability to create it still surfaces loudly and
//     attributably at the very next `issue create`, which names the label.
//
// It runs once per store: repeating it before every create would spend a call
// per issue on a question already answered.
func (g *ghStore) ensureLabel() error {
	if g.labelEnsured {
		return nil
	}

	for _, l := range createLabels {
		if !l.owned {
			// Best-effort: see the ignored-error rationale above.
			_, _ = g.run("label", "create", l.name, "--repo", g.repo)

			continue
		}

		// --color is not optional here: without it --force re-rolls a random
		// colour on every run that reaches this path. See createLabels.color.
		if _, err := g.run("label", "create", l.name,
			"--repo", g.repo,
			"--description", l.description,
			"--color", l.color,
			"--force"); err != nil {
			return err
		}
	}

	g.labelEnsured = true

	return nil
}

// create files a new backlog issue. The issue number comes out of the URL
// `gh issue create` prints, since `gh` offers no JSON output for create.
func (g *ghStore) create(title, body string) (int, error) {
	if err := g.ensureLabel(); err != nil {
		return 0, err
	}

	args := []string{"issue", "create",
		"--repo", g.repo,
		"--title", title,
		"--body", body}
	for _, l := range createLabels {
		args = append(args, "--label", l.name)
	}

	raw, err := g.run(args...)
	if err != nil {
		return 0, err
	}

	number, err := parseIssueNumber(string(raw))
	if err != nil {
		return 0, err
	}

	return number, nil
}

func (g *ghStore) update(number int, title, body string) error {
	_, err := g.run("issue", "edit", fmt.Sprint(number),
		"--repo", g.repo, "--title", title, "--body", body)

	return err
}

func (g *ghStore) reopen(number int, title, body string) error {
	if _, err := g.run("issue", "reopen", fmt.Sprint(number), "--repo", g.repo); err != nil {
		return err
	}

	return g.update(number, title, body)
}

func (g *ghStore) close(number int, comment, reason string) error {
	_, err := g.run("issue", "close", fmt.Sprint(number),
		"--repo", g.repo, "--comment", comment, "--reason", reason)

	return err
}

// apiStateReason maps this command's disposition vocabulary onto the REST one.
//
// `gh issue close --reason` spells it "not planned" with a SPACE; the REST
// state_reason field spells it not_planned with an UNDERSCORE and rejects the
// spaced form. The close path never meets this because it goes through the CLI
// flag, so the mismatch would surface only on the reclassify path — as a 422 on
// exactly the transition this exists to record.
func apiStateReason(disposition string) string {
	if disposition == dispositionNotPlanned {
		return "not_planned"
	}

	return "completed"
}

// reclassify corrects an already-closed issue's state_reason and comments to
// explain the correction.
//
// Two calls rather than one because no `gh issue` subcommand sets state_reason
// on an issue that is already closed: `close` refuses it and `edit` has no such
// flag, so the structured half goes through the REST API directly.
//
// The STATE CHANGE goes first, and the ordering is the whole safety property.
// This runs on every closed entry forever, and planWrites derives the same
// action from the same inputs each time, so whichever call fails is retried on
// the next run:
//
//   - PATCH first (this order): a failed PATCH has posted nothing, and the next
//     run re-derives the same disagreement and retries both calls. Idempotent.
//   - Comment first (the previous order): a failed PATCH leaves state_reason
//     unchanged, so the next run derives the same disagreement and posts the
//     same comment AGAIN. Repeated failures accumulate duplicate comments on a
//     settled issue, unbounded.
//
// The cost of this order is the reverse failure: if the PATCH succeeds and the
// comment fails, the disposition is corrected with nothing on the issue saying
// why, and the next run sees agreement so it never explains it. That loss is
// bounded to one occurrence and destroys nothing, whereas comment spam on a
// settled issue is unbounded — so it is the better direction to fail in.
// state accompanies state_reason because the REST API IGNORES state_reason
// unless state is submitted in the same request. Reclassification only ever
// targets an already-closed issue, so state=closed re-asserts the state the
// issue is already in and the disposition is what actually changes. Omitting it
// makes the PATCH succeed while changing nothing — which is indistinguishable
// from success here, so the next run re-derives the same disagreement and the
// retry loop this function's ordering exists to bound never terminates.
func (g *ghStore) reclassify(number int, comment, reason string) error {
	if _, err := g.run("api", "--method", "PATCH",
		fmt.Sprintf("repos/%s/issues/%d", g.repo, number),
		"-f", "state=closed",
		"-f", "state_reason="+apiStateReason(reason)); err != nil {
		return err
	}

	_, err := g.run("issue", "comment", fmt.Sprint(number),
		"--repo", g.repo, "--body", comment)

	return err
}

// parseIssueNumber extracts the trailing number from a `gh issue create` URL.
func parseIssueNumber(output string) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "/issues/") {
			continue
		}

		var number int
		if _, err := fmt.Sscanf(line[strings.LastIndex(line, "/")+1:], "%d", &number); err == nil {
			return number, nil
		}
	}

	return 0, fmt.Errorf("%w: no issue URL in `gh issue create` output %q", errWriteFailed, output)
}
