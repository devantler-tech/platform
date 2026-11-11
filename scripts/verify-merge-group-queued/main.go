// Command verify-merge-group-queued pins the active queue entry at the start
// of a run and rechecks it immediately before production access. GitHub squash
// groups do not expose the source head in their ref or commit parents.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const repository = "devantler-tech/platform"

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var queueRefPattern = regexp.MustCompile(`^refs/heads/gh-readonly-queue/main/pr-([1-9][0-9]*)-[0-9a-f]{40}$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,200}$`)
var errQueueTransport = errors.New("bounded queue or checkout read failed")

type event struct {
	Action     string
	Repository struct {
		FullName string `json:"full_name"`
	}
	Group struct {
		BaseRef string                         `json:"base_ref"`
		BaseSHA string                         `json:"base_sha"`
		HeadRef string                         `json:"head_ref"`
		HeadSHA string                         `json:"head_sha"`
		Commit  struct{ ID, Timestamp string } `json:"head_commit"`
	} `json:"merge_group"`
}

type receipt struct {
	Repository, Ref, Group, Base, Entry, Source string
	PR                                          int
}

type oid struct{ OID string }

type witness struct {
	receipt
	main     string
	position int
}

type response struct {
	Errors json.RawMessage
	Data   struct {
		Repository struct {
			NameWithOwner string
			Ref           struct{ Target oid }
			Main          struct{ Target oid }
			MergeQueue    struct{ ID string }
			PullRequest   struct {
				Number                         int
				State, BaseRefName, HeadRefOID string
				IsInMergeQueue                 bool
				Entry                          struct {
					ID, EnqueuedAt         string
					Position               int
					HeadCommit, BaseCommit oid
					MergeQueue             struct{ ID string }
					PullRequest            struct {
						Number     int
						HeadRefOID string
					}
				} `json:"mergeQueueEntry"`
			}
		}
	}
}

// utc validates RFC 3339 and normalizes offsets before queue times are compared.
func utc(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("invalid queue timestamp")
	}
	return t.UTC(), nil
}

// parseEvent validates the trusted Actions identity before any network read.
// Only the PR number is extracted from the ref; its SHA suffix is not a source head.
func parseEvent(data []byte, repo, sha, name string) (event, int, error) {
	var e event
	if err := json.Unmarshal(data, &e); err != nil {
		return e, 0, errors.New("unreadable merge-group event")
	}
	match := queueRefPattern.FindStringSubmatch(e.Group.HeadRef)
	if name != "merge_group" || repo != repository || e.Repository.FullName != repository ||
		e.Action != "checks_requested" || e.Group.BaseRef != "refs/heads/main" ||
		!shaPattern.MatchString(e.Group.BaseSHA) || !shaPattern.MatchString(sha) ||
		e.Group.HeadSHA != sha || e.Group.Commit.ID != sha || match == nil {
		return e, 0, errors.New("event does not bind this repository, base and merge-group revision")
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return e, 0, errors.New("invalid queued pull request number")
	}
	if _, err := utc(e.Group.Commit.Timestamp); err != nil {
		return e, 0, err
	}
	return e, number, nil
}

// evaluate requires a complete, error-free association between the current PR,
// its queue entry, the base commit and the exact synthetic Git ref.
func evaluate(data []byte, e event, number int) (witness, error) {
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return witness{}, errors.New("unreadable queue witness")
	}
	if len(r.Errors) > 0 && string(r.Errors) != "null" && string(r.Errors) != "[]" {
		return witness{}, errors.New("queue read contains GraphQL errors")
	}
	repo := r.Data.Repository
	pr, entry := repo.PullRequest, repo.PullRequest.Entry
	enqueued, err := utc(entry.EnqueuedAt)
	if err != nil {
		return witness{}, err
	}
	created, err := utc(e.Group.Commit.Timestamp)
	if err != nil {
		return witness{}, err
	}
	if repo.NameWithOwner != repository || pr.Number != number || pr.State != "OPEN" ||
		pr.BaseRefName != "main" || !pr.IsInMergeQueue ||
		!idPattern.MatchString(entry.ID) || !idPattern.MatchString(repo.MergeQueue.ID) ||
		entry.MergeQueue.ID != repo.MergeQueue.ID || entry.PullRequest.Number != number ||
		!shaPattern.MatchString(pr.HeadRefOID) || entry.PullRequest.HeadRefOID != pr.HeadRefOID ||
		entry.HeadCommit.OID != e.Group.HeadSHA || entry.BaseCommit.OID != e.Group.BaseSHA ||
		repo.Ref.Target.OID != e.Group.HeadSHA || enqueued.After(created) {
		return witness{}, errors.New("merge group is absent, replaced or inconsistent with the active queue")
	}
	return witness{
		receipt:  receipt{repository, e.Group.HeadRef, e.Group.HeadSHA, e.Group.BaseSHA, entry.ID, pr.HeadRefOID, number},
		main:     repo.Main.Target.OID,
		position: entry.Position,
	}, nil
}

// command bounds each read, disables interactive authentication and never emits
// raw API or credential diagnostics to the public workflow log.
func command(ctx context.Context, workspace, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // Names and arguments are fixed by callers; no shell.
	cmd.Dir = workspace
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if out.Len() == 0 {
			return nil, errQueueTransport
		}
		return nil, errors.New("queue or checkout command returned an unsuccessful response")
	}
	if out.Len() > 4<<20 {
		return nil, errors.New("queue read is too large")
	}
	return out.Bytes(), nil
}

// The same query ties the PR entry to its queue and the actual synthetic ref.
const query = `query($number:Int!,$ref:String!){repository(owner:"devantler-tech",name:"platform"){nameWithOwner ref(qualifiedName:$ref){target{oid}} main:ref(qualifiedName:"refs/heads/main"){target{oid}} mergeQueue(branch:"main"){id} pullRequest(number:$number){number state baseRefName headRefOid isInMergeQueue mergeQueueEntry{id enqueuedAt position headCommit{oid} baseCommit{oid} mergeQueue{id} pullRequest{number headRefOid}}}}}`

// readQueue retries failed transport at most three times within the existing
// deadline. A returned witness is evaluated once; refusals are never retried.
func readQueue(ctx context.Context, workspace string, number int, ref string) ([]byte, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var data []byte
		data, err = command(ctx, workspace, "gh", "api", "--hostname", "github.com", "graphql", "-f", "query="+query, "-F", "number="+strconv.Itoa(number), "-f", "ref="+ref)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, errQueueTransport) {
			return nil, err
		}
		if attempt == 2 || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w: read deadline expired", errQueueTransport)
		case <-timer.C:
		}
	}
	return nil, err
}

// waitForTurn polls only while the same candidate is valid but is still behind
// another entry or main has not advanced to its parent. The production job
// calls this once more without polling, after acquiring the shared lock.
func waitForTurn(ctx context.Context, read func() (witness, error), initial receipt, parent string, interval time.Duration, stdout io.Writer) error {
	waiting := false
	for {
		current, err := read()
		switch {
		case err != nil && (interval == 0 || !errors.Is(err, errQueueTransport)):
			return err
		case err != nil:
			// No response to evaluate. Use the next poll within the same deadline.
		case current.receipt != initial:
			return errors.New("initial queue entry or source head was replaced")
		case current.position < 1 || !shaPattern.MatchString(current.main):
			return errors.New("missing queue position or main revision")
		case current.position == 1 && current.main == parent:
			return nil
		}
		if interval == 0 {
			return errors.New("merge group is not first on the current main revision")
		}
		if !waiting {
			_, _ = fmt.Fprintln(stdout, "Waiting for queue admission on the current main revision.")
			waiting = true
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("timed out waiting for the production deployment turn")
		case <-timer.C:
		}
	}
}

// verify captures the run's initial source identity or proves its turn to deploy.
func verify(mode string, stdout io.Writer) error {
	if mode != "capture" && mode != "wait" && mode != "check" {
		return errors.New("usage: verify-merge-group-queued <capture|wait|check>")
	}
	file, err := os.Open(os.Getenv("GITHUB_EVENT_PATH")) //nolint:gosec // Trusted Actions event file.
	if err != nil {
		return errors.New("merge-group event is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	_ = file.Close()
	if err != nil || len(data) > 4<<20 {
		return errors.New("merge-group event is unreadable")
	}
	e, number, err := parseEvent(data, os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_EVENT_NAME"))
	if err != nil {
		return err
	}
	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace == "" {
		return errors.New("checkout workspace is missing")
	}
	if os.Getenv("GH_TOKEN") == "" {
		return errors.New("ephemeral queue read token is missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	head, err := command(ctx, workspace, "git", "--no-replace-objects", "-c", "core.fsmonitor=false", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(head)) != e.Group.HeadSHA {
		return errors.New("checkout does not match the merge group")
	}
	dirty, err := command(ctx, workspace, "git", "--no-replace-objects", "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || len(dirty) != 0 {
		return errors.New("checkout is not clean")
	}
	deadline := 30 * time.Second
	if mode == "wait" {
		deadline = 85 * time.Minute
	}
	queueContext, cancelQueue := context.WithTimeout(context.Background(), deadline)
	defer cancelQueue()
	read := func() (witness, error) {
		readContext, cancelRead := context.WithTimeout(queueContext, 30*time.Second)
		defer cancelRead()
		reply, err := readQueue(readContext, workspace, number, e.Group.HeadRef)
		if err != nil {
			return witness{}, err
		}
		return evaluate(reply, e, number)
	}
	if mode != "capture" {
		var initial receipt
		if err := json.Unmarshal([]byte(os.Getenv("QUEUE_RECEIPT")), &initial); err != nil {
			return errors.New("initial queue receipt is missing or unreadable")
		}
		revision, err := command(ctx, workspace, "git", "--no-replace-objects", "-c", "core.fsmonitor=false", "rev-list", "--parents", "-n", "1", "HEAD")
		parents := strings.Fields(string(revision))
		if err != nil || len(parents) != 2 || parents[0] != e.Group.HeadSHA || !shaPattern.MatchString(parents[1]) {
			return errors.New("merge group does not have exactly one verifiable parent")
		}
		var interval time.Duration
		if mode == "wait" {
			interval = 20 * time.Second
		}
		if err := waitForTurn(queueContext, read, initial, parents[1], interval, stdout); err != nil {
			return err
		}
	} else {
		current, err := read()
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(current.receipt)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // Actions output file.
		if err != nil {
			return errors.New("queue receipt output is unavailable")
		}
		_, err = fmt.Fprintf(out, "receipt=%s\n", encoded)
		closeErr := out.Close()
		if err != nil || closeErr != nil {
			return errors.New("queue receipt could not be written")
		}
	}
	_, err = fmt.Fprintf(stdout, "Active merge-group queue witness verified for #%d (%s).\n", number, mode)
	return err
}

// main preserves refusal as a failing step so production access cannot follow it.
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: verify-merge-group-queued <capture|wait|check>")
		os.Exit(2)
	}
	if err := verify(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Merge-group production preflight refused:", err)
		os.Exit(1)
	}
}
