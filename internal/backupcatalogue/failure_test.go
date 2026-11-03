package backupcatalogue

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestFailureStageReportsOnlyFixedPhasesAndDiscardsPrivateErrors(t *testing.T) {
	for _, phase := range []string{"deployment-read-before", "deployment-bind-before", "repository-metadata", "deployment-read-after", "deployment-bind-after", "deployment-changed"} {
		t.Run(phase, func(t *testing.T) {
			policy, docs := bindingFixture(t, "wedding-app")
			loads := 0
			load := func(context.Context) (DeploymentDocuments, error) {
				loads++
				if phase == "deployment-read-before" && loads == 1 || phase == "deployment-read-after" && loads == 2 {
					return DeploymentDocuments{}, errors.New("private credential and endpoint")
				}
				if phase == "deployment-bind-before" && loads == 1 || phase == "deployment-bind-after" && loads == 2 {
					return DeploymentDocuments{}, nil
				}
				if phase == "deployment-changed" && loads == 2 {
					docs.Repository = changeBinding(t, docs.Repository, func(v map[string]any) { v["metadata"].(map[string]any)["resourceVersion"] = "124" })
				}
				return docs, nil
			}
			read := func(context.Context, Policy, string, string, string, string, http.RoundTripper) (Catalogue, error) {
				if phase == "repository-metadata" {
					return Catalogue{}, errors.New("private credential and endpoint")
				}
				return Catalogue{MetadataOnly: true, Digest: "partial-result"}, nil
			}
			result, err := observeBoundDeployment(context.Background(), "wedding-app", policy, load, t.TempDir(), http.DefaultTransport, read)
			if !errors.Is(err, errReadHeld) || FailureStage(err) != phase || strings.Contains(err.Error(), "private") || result.Digest != "" {
				t.Fatal("wrong phase, private error or partial receipt escaped")
			}
		})
	}
	if FailureStage(errors.New("deployment-read-before: private credential")) != "observation" || FailureStage(nil) != "observation" {
		t.Fatal("untyped error text became a public phase")
	}
}
