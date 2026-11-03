package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/backupcatalogue"
)

func TestRunReportsSafeStageWithoutPublishingPartialReceipt(t *testing.T) {
	env := authorizedEnvironment()
	for _, phase := range []string{"source-before", "observation", "receipt-validation", "source-after"} {
		t.Run(phase, func(t *testing.T) {
			checks := 0
			execute := func(context.Context, string, ...string) ([]byte, error) {
				checks++
				if phase == "source-before" && checks == 1 || phase == "source-after" && checks == 3 {
					return nil, errors.New("private endpoint")
				}
				return []byte(env["GITHUB_SHA"] + "\trefs/heads/main\n"), nil
			}
			observe := func(context.Context, string, backupcatalogue.Policy, func(context.Context) (backupcatalogue.DeploymentDocuments, error), string) (backupcatalogue.Catalogue, error) {
				if phase == "observation" {
					return backupcatalogue.Catalogue{}, errors.New("private credential")
				}
				return backupcatalogue.Catalogue{MetadataOnly: phase != "receipt-validation", Digest: strings.Repeat("c", 64), RepositoryDigest: strings.Repeat("d", 64)}, nil
			}
			result, err := run(context.Background(), func(k string) string { return env[k] }, execute, observe, backupcatalogue.Policy{}, t.TempDir())
			if !errors.Is(err, errHeld) || failureStage(err) != phase || strings.Contains(err.Error(), "private") || len(result.Repositories) != 0 {
				t.Fatal("unsafe stage or partial receipt")
			}
		})
	}
	if failureStage(errors.New("source-before: private")) != "unspecified" {
		t.Fatal("untyped provider text became a stage")
	}
}
