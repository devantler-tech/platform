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
	close(number int, comment string) error
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

	if p.empty() {
		_, err := fmt.Fprintln(out, "no changes: every derived theme already matches its tracked issue")

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
			if err = store.close(a.Number, a.Reason); err == nil {
				_, err = fmt.Fprintf(out, "closed #%d\n", a.Number)
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
		"--json", "number,title,body,state")
	if err != nil {
		return nil, err
	}

	var issues []ghIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("decoding gh issue list: %w", err)
	}

	if len(issues) >= listLimit {
		return nil, fmt.Errorf("%w: %d tracked issues hit the --limit %d ceiling, so the listing "+
			"may be truncated and every omitted theme would be re-filed as a duplicate",
			errWriteFailed, len(issues), listLimit)
	}

	entries := make([]backlogEntry, 0, len(issues))
	for _, i := range issues {
		entries = append(entries, backlogEntry{
			Number: i.Number,
			Title:  i.Title,
			Body:   i.Body,
			Open:   strings.EqualFold(i.State, "open"),
		})
	}

	return entries, nil
}

// createdNumberPattern pulls the issue number out of the URL `gh issue create`
// prints. `gh` offers no JSON output for create, so the URL is the only handle
// it returns.
func (g *ghStore) create(title, body string) (int, error) {
	raw, err := g.run("issue", "create",
		"--repo", g.repo,
		"--title", title,
		"--body", body,
		"--label", bridgeLabel,
		"--label", "security")
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

func (g *ghStore) close(number int, comment string) error {
	_, err := g.run("issue", "close", fmt.Sprint(number),
		"--repo", g.repo, "--comment", comment)

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
