package backupcatalogue

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestBoundObservationDiscardsChangedOrIncompleteBindings(t *testing.T) {
	for _, mode := range []string{"stable", "validation-heartbeat", "wrong-before", "failed-after", "changed-after", "expired-after", "reader-failed"} {
		t.Run(mode, func(t *testing.T) {
			p, docs := bindingFixture(t, "wedding-app")
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			calls, observed := 0, 0
			load := func(context.Context) (DeploymentDocuments, error) {
				calls++
				if mode == "wrong-before" {
					return DeploymentDocuments{}, nil
				}
				if calls == 2 {
					switch mode {
					case "validation-heartbeat":
						docs.Location = changeBinding(t, docs.Location, func(v map[string]any) {
							v["metadata"].(map[string]any)["resourceVersion"] = "124"
							v["metadata"].(map[string]any)["generation"] = float64(2)
							v["status"].(map[string]any)["lastValidationTime"] = "2026-11-02T23:53:44Z"
						})
					case "failed-after":
						return DeploymentDocuments{}, errors.New("private provider detail")
					case "changed-after":
						docs.Repository = changeBinding(t, docs.Repository, func(v map[string]any) { v["metadata"].(map[string]any)["uid"] = "replacement-repository-uid" })
					case "expired-after":
						cancel()
					}
				}
				return docs, nil
			}
			read := func(_ context.Context, p Policy, access, secret, password, _ string, _ http.RoundTripper) (Catalogue, error) {
				observed++
				if p.Prefix != "velero/prod/kopia/wedding-app/" || access != "fixture-access" || secret != "fixture-secret" || password != "fixture-password" {
					t.Fatal("wrong read scope")
				}
				if mode == "reader-failed" {
					return Catalogue{}, errors.New("private provider detail")
				}
				return Catalogue{MetadataOnly: true, Snapshots: 1, Digest: "fixture-digest"}, nil
			}
			result, err := observeBoundDeployment(ctx, "wedding-app", p, load, t.TempDir(), http.DefaultTransport, read)
			if mode == "stable" || mode == "validation-heartbeat" {
				if err != nil || result.Snapshots != 1 || calls != 2 || observed != 1 {
					t.Fatal("stable complete observation refused")
				}
				return
			}
			if !errors.Is(err, errReadHeld) || result.Snapshots != 0 || result.Digest != "" || len(result.entries) != 0 {
				t.Fatal("partial result or provider error escaped")
			}
			if mode == "wrong-before" && observed != 0 {
				t.Fatal("access before binding")
			}
		})
	}
}
