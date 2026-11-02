// Command observe-historical-longhorn-backups reads bounded Kopia metadata for
// the two repositories relevant to the retained June Longhorn snapshots. It
// prints aggregate metadata receipts only, after both repositories and current
// source identity have been checked. It grants no recovery or reset clearance.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/devantler-tech/platform/internal/backupcatalogue"
)

var errHeld = errors.New("historical backup metadata observation held; no recovery or reset clearance")
var sourceIdentity = regexp.MustCompile(`^[0-9a-f]{40}$`)
var receiptDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type command func(context.Context, string, ...string) ([]byte, error)
type observer func(context.Context, string, backupcatalogue.Policy, func(context.Context) (backupcatalogue.DeploymentDocuments, error), string) (backupcatalogue.Catalogue, error)

type repositoryReceipt struct {
	Consumer                       string `json:"consumer"`
	MetadataCandidatesBeforeJune17 int    `json:"metadataCandidatesBefore20260617"`
	backupcatalogue.Catalogue
}
type receipt struct {
	MetadataOnly bool                `json:"metadataOnly"`
	Source       string              `json:"source"`
	Run          string              `json:"run"`
	Repositories []repositoryReceipt `json:"repositories"`
}

func active(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	deadline, bounded := ctx.Deadline()
	return !bounded || time.Now().Before(deadline)
}

func authorize(get func(string) string) error {
	for key, expected := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF":        "devantler-tech/platform/.github/workflows/observe-historical-longhorn-backups.yaml@refs/heads/main",
		"LONGHORN_CATALOGUE_CONFIRM": "observe-historical-longhorn-backups",
	} {
		if get(key) != expected {
			return errHeld
		}
	}
	if !sourceIdentity.MatchString(get("GITHUB_SHA")) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(get("GITHUB_RUN_ID")) {
		return errHeld
	}
	return nil
}

func requireSource(ctx context.Context, execute command, expected string) error {
	if !active(ctx) {
		return errHeld
	}
	data, err := execute(ctx, "git", "ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main")
	if err != nil || !active(ctx) || !sourceIdentity.MatchString(expected) || string(data) != expected+"\trefs/heads/main\n" {
		return errHeld
	}
	return nil
}

func run(ctx context.Context, get func(string) string, execute command, observe observer, policy backupcatalogue.Policy, parent string) (receipt, error) {
	if authorize(get) != nil || !active(ctx) || execute == nil || observe == nil {
		return receipt{}, errHeld
	}
	result := receipt{MetadataOnly: true, Source: get("GITHUB_SHA"), Run: get("GITHUB_RUN_ID")}
	for _, namespace := range []string{"wedding-app", "actual-budget"} {
		if requireSource(ctx, execute, result.Source) != nil {
			return receipt{}, observationFailure{"source-before"}
		}
		load := func(ctx context.Context) (backupcatalogue.DeploymentDocuments, error) {
			return readDocuments(ctx, execute, namespace)
		}
		catalogue, err := observe(ctx, namespace, policy, load, parent)
		if err != nil {
			return receipt{}, observationFailure{backupcatalogue.FailureStage(err)}
		}
		if !active(ctx) || !catalogue.MetadataOnly || !receiptDigest.MatchString(catalogue.Digest) || !receiptDigest.MatchString(catalogue.RepositoryDigest) ||
			catalogue.Snapshots < 0 || catalogue.Incomplete < 0 || catalogue.Incomplete > catalogue.Snapshots {
			return receipt{}, observationFailure{"receipt-validation"}
		}
		result.Repositories = append(result.Repositories, repositoryReceipt{Consumer: namespace, Catalogue: catalogue,
			MetadataCandidatesBeforeJune17: catalogue.MetadataEndingBefore(time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC))})
	}
	if requireSource(ctx, execute, result.Source) != nil || !active(ctx) {
		return receipt{}, observationFailure{"source-after"}
	}
	return result, nil
}

func main() {
	if authorize(os.Getenv) != nil {
		fmt.Fprintln(os.Stderr, observationFailure{"authorization"})
		os.Exit(1)
	}
	if requireCredentialFiles(os.Getenv) != nil {
		fmt.Fprintln(os.Stderr, observationFailure{"credential-files"})
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
	defer cancel()
	policy, err := reviewedPolicy(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, observationFailure{"reviewed-policy"})
		os.Exit(1)
	}
	result, err := run(ctx, os.Getenv, executeRead, backupcatalogue.ObserveDeployment, policy, os.Getenv("LONGHORN_CATALOGUE_CREDENTIAL_DIR"))
	if err != nil {
		fmt.Fprintln(os.Stderr, observationFailure{failureStage(err)})
		os.Exit(1)
	}
	if !active(ctx) || json.NewEncoder(os.Stdout).Encode(result) != nil {
		fmt.Fprintln(os.Stderr, observationFailure{"receipt-output"})
		os.Exit(1)
	}
}
