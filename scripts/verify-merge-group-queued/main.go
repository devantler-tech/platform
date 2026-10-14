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
type response struct {
	Errors json.RawMessage
	Data   struct {
		Repository struct {
			NameWithOwner string
			Ref           struct{ Target oid }
			MergeQueue    struct{ ID string }
			PullRequest   struct {
				Number                         int
				State, BaseRefName, HeadRefOID string
				IsInMergeQueue                 bool
				Entry                          struct {
					ID, EnqueuedAt         string
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
func evaluate(data []byte, e event, number int) (receipt, error) {
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return receipt{}, errors.New("unreadable queue witness")
	}
	if len(r.Errors) > 0 && string(r.Errors) != "null" && string(r.Errors) != "[]" {
		return receipt{}, errors.New("queue read contains GraphQL errors")
	}
	repo := r.Data.Repository
	pr, entry := repo.PullRequest, repo.PullRequest.Entry
	enqueued, err := utc(entry.EnqueuedAt)
	if err != nil {
		return receipt{}, err
	}
	created, err := utc(e.Group.Commit.Timestamp)
	if err != nil {
		return receipt{}, err
	}
	if repo.NameWithOwner != repository || pr.Number != number || pr.State != "OPEN" ||
		pr.BaseRefName != "main" || !pr.IsInMergeQueue ||
		!idPattern.MatchString(entry.ID) || !idPattern.MatchString(repo.MergeQueue.ID) ||
		entry.MergeQueue.ID != repo.MergeQueue.ID || entry.PullRequest.Number != number ||
		!shaPattern.MatchString(pr.HeadRefOID) || entry.PullRequest.HeadRefOID != pr.HeadRefOID ||
		entry.HeadCommit.OID != e.Group.HeadSHA || entry.BaseCommit.OID != e.Group.BaseSHA ||
		repo.Ref.Target.OID != e.Group.HeadSHA || enqueued.After(created) {
		return receipt{}, errors.New("merge group is absent, replaced or inconsistent with the active queue")
	}
	return receipt{repository, e.Group.HeadRef, e.Group.HeadSHA, e.Group.BaseSHA, entry.ID, pr.HeadRefOID, number}, nil
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
		return nil, errors.New("bounded queue or checkout read failed")
	}
	if out.Len() > 4<<20 {
		return nil, errors.New("queue read is too large")
	}
	return out.Bytes(), nil
}

// The same query ties the PR entry to its queue and the actual synthetic ref.
const query = `query($number:Int!,$ref:String!){repository(owner:"devantler-tech",name:"platform"){nameWithOwner ref(qualifiedName:$ref){target{oid}} mergeQueue(branch:"main"){id} pullRequest(number:$number){number state baseRefName headRefOid isInMergeQueue mergeQueueEntry{id enqueuedAt headCommit{oid} baseCommit{oid} mergeQueue{id} pullRequest{number headRefOid}}}}}`

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
		if attempt == 2 || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.New("queue read deadline expired")
		case <-timer.C:
		}
	}
	return nil, err
}

// verify captures the run's initial source identity or rechecks that receipt.
// It does not infer a historical source head from the event or commit ancestry.
func verify(mode string, stdout io.Writer) error {
	if mode != "capture" && mode != "check" {
		return errors.New("usage: verify-merge-group-queued <capture|check>")
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
	if mode == "check" && os.Getenv("QUEUE_RECEIPT") == "" {
		return errors.New("initial queue receipt is missing")
	}
	data, err = readQueue(ctx, workspace, number, e.Group.HeadRef)
	if err != nil {
		return err
	}
	current, err := evaluate(data, e, number)
	if err != nil {
		return err
	}
	if mode == "check" {
		var initial receipt
		if err := json.Unmarshal([]byte(os.Getenv("QUEUE_RECEIPT")), &initial); err != nil || initial != current {
			return errors.New("initial queue entry or source head was replaced")
		}
	} else {
		encoded, err := json.Marshal(current)
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
		fmt.Fprintln(os.Stderr, "usage: verify-merge-group-queued <capture|check>")
		os.Exit(2)
	}
	if err := verify(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Merge-group production preflight refused:", err)
		os.Exit(1)
	}
}
