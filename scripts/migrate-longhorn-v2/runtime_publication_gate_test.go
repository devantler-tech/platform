package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runtimePublicationEnvironment() map[string]string {
	ref := "refs/heads/gh-readonly-queue/main/pr-123-" + strings.Repeat("a", 40)
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_EVENT_NAME": "merge_group", "GITHUB_REF": ref,
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/ci.yaml@" + ref, "GITHUB_JOB": "deploy-prod", "GITHUB_SHA": strings.Repeat("b", 40)}
}

func TestRuntimePublicationBindsActualCheckoutAndCurrentMainBase(t *testing.T) {
	for _, tc := range []struct{ name, change, want string }{
		{"queue", "", "queue"}, {"healing current main", "heal", "recovery"}, {"manual CD", "cd", "recovery"}, {"cold DR", "dr", "recovery"},
		{"wrong checkout", "head", ""}, {"unknown job", "job", ""}, {"unbound workflow", "workflow", ""}, {"unbound ref", "ref", ""}, {"wrong event", "event", ""},
		{"missing base", "base", ""}, {"wrong head", "event head", ""}, {"wrong base branch", "base ref", ""}, {"missing run", "run", ""},
		{"duplicate payload key", "duplicate", ""}, {"incomplete event", "missing event", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := runtimePublicationEnvironment()
			head := env["GITHUB_SHA"]
			group := object{"base_sha": strings.Repeat("c", 40), "head_sha": head, "base_ref": "refs/heads/main", "head_ref": env["GITHUB_REF"]}
			switch tc.change {
			case "heal":
				env["GITHUB_JOB"], env["LONGHORN_RUNTIME_RECOVERY_SOURCE"] = "heal-prod-on-failure", strings.Repeat("d", 40)
				head = strings.Repeat("d", 40)
			case "cd", "dr":
				file, job := "cd.yaml", "deploy-prod"
				if tc.change == "dr" {
					file, job = "dr-rebuild.yaml", "rebuild"
				}
				env["GITHUB_EVENT_NAME"], env["GITHUB_REF"], env["GITHUB_JOB"] = "workflow_dispatch", "refs/heads/main", job
				env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/" + file + "@refs/heads/main"
				env["LONGHORN_RUNTIME_RECOVERY_SOURCE"] = head
			case "head":
				head = strings.Repeat("e", 40)
			case "job":
				env["GITHUB_JOB"] = "validate"
			case "workflow":
				env["GITHUB_WORKFLOW_REF"] = "other"
			case "ref":
				env["GITHUB_REF"] = "refs/heads/unreviewed"
			case "event":
				env["GITHUB_EVENT_NAME"] = "pull_request"
			case "base":
				group["base_sha"] = ""
			case "event head":
				group["head_sha"] = strings.Repeat("f", 40)
			case "base ref":
				group["base_ref"] = "refs/heads/other"
			case "run":
				env["GITHUB_RUN_ID"] = ""
			}
			event, err := json.Marshal(object{"merge_group": group})
			if err != nil {
				t.Fatal(err)
			}
			if tc.change == "duplicate" {
				event = []byte(`{"merge_group":{},"merge_group":{}}`)
			}
			if tc.change == "missing event" {
				event = nil
			}
			got, err := runtimePublicationContext(func(k string) string { return env[k] }, event, head)
			if tc.want == "" {
				if err == nil {
					t.Fatal("unbound publication was authorized")
				}
			} else if err != nil || got.Kind != tc.want || got.Candidate != head || got.Base == env["GITHUB_SHA"] && tc.want == "queue" {
				t.Fatalf("context = %+v %v", got, err)
			}
		})
	}
}

func TestRuntimePublicationInputBindingDetectsChangesAndLinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "kustomization.yaml")
	if err := os.WriteFile(file, []byte("kind: Kustomization\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := runtimePublicationFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("kind: Other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := runtimePublicationFingerprint(root)
	if err != nil || before == after {
		t.Fatal("source edit was not detected")
	}
	if err := os.Symlink(file, filepath.Join(root, "indirect.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimePublicationFingerprint(root); err == nil {
		t.Fatal("indirect producer input accepted")
	}
}

func TestRuntimePublicationFirstSelectionIsOnlyTheReviewedComponent(t *testing.T) {
	base := []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [one]\ncomponents: [existing]\n")
	candidate := []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [one]\ncomponents: [existing, ../../../../bases/components/longhorn-v1-retirement-runtime]\n")
	if err := runtimePublicationActivation(base, candidate); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{base, []byte(strings.ReplaceAll(string(candidate), "[one]", "[two]")), []byte(strings.ReplaceAll(string(candidate), "existing,", "other,")), append(candidate, []byte("---\nkind: Kustomization\n")...)} {
		if runtimePublicationActivation(base, bad) == nil {
			t.Fatal("mixed or missing activation accepted")
		}
	}
}

func TestRuntimePublicationActivationKeepsItsMatchingAuthorizationApproval(t *testing.T) {
	paths := runtimeControllersRoot + "\n" + runtimePublicationApprovalPath + "\n"
	if !runtimePublicationChangeScope([]byte(paths)) {
		t.Fatal("required component and authorization update rejected")
	}
	for _, bad := range []string{runtimeControllersRoot + "\n", runtimePublicationApprovalPath + "\n", paths + "k8s/other.yaml\n", paths + runtimePublicationApprovalPath + "\n"} {
		if runtimePublicationChangeScope([]byte(bad)) {
			t.Fatal("missing or unrelated activation change accepted")
		}
	}
	prefix := "helm.toolkit.fluxcd.io/v2|HelmRelease|longhorn-system|longhorn "
	base := []byte("# Approved surface\n\n" + prefix + strings.Repeat("a", 64) + "\n\nother-entry " + strings.Repeat("c", 64) + "\n")
	candidate := []byte(strings.ReplaceAll(string(base), prefix+strings.Repeat("a", 64), prefix+strings.Repeat("b", 64)))
	if runtimePublicationApprovalDelta(base, candidate) != nil {
		t.Fatal("sole matching approval rejected")
	}
	for _, bad := range [][]byte{base, append(candidate, []byte("extra-entry "+strings.Repeat("d", 64)+"\n")...), []byte(strings.ReplaceAll(string(candidate), "other-entry "+strings.Repeat("c", 64), "other-entry "+strings.Repeat("d", 64))), append(candidate, []byte(prefix+strings.Repeat("e", 64)+"\n")...), []byte(strings.ReplaceAll(string(candidate), strings.Repeat("b", 64), "invalid"))} {
		if runtimePublicationApprovalDelta(base, bad) == nil {
			t.Fatal("unrelated or malformed authorization update accepted")
		}
	}
}
