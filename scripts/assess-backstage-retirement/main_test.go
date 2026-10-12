package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const deploymentManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: backstage
  namespace: backstage
spec:
  template:
    spec:
      containers:
        - name: backstage
          image: example.invalid/backstage:1
      volumes:
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: backstage
  namespace: backstage
`

// fixture separates the synthetic installed release from retained database data.
func fixture() (map[string]any, map[string]any) {
	release := map[string]any{
		"name": "backstage", "namespace": "backstage", "version": 4,
		"info":     map[string]any{"status": "deployed"},
		"manifest": deploymentManifest,
		"config":   map[string]any{"fixture_marker": "DO_NOT_PRINT"},
	}
	live := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata("backstage", "deployment-uid"), "spec": map[string]any{
			"selector": map[string]any{"matchLabels": portalLabels()},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "backstage"}}, "volumes": []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{}}}}},
		}},
		map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": podMetadata(), "spec": map[string]any{
			"containers": []any{map[string]any{"name": "backstage"}}, "volumes": []any{map[string]any{"name": "config", "configMap": map[string]any{"name": "backstage-app-config"}}},
		}},
		map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": metadata("backstage-db", "database-uid")},
		map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": metadata("backstage-db-3", "claim-uid")},
	}}
	object(release["info"])["resources"] = map[string]any{
		"v1/Deployment": []any{live["items"].([]any)[0]},
		"v1/Service": []any{map[string]any{"apiVersion": "v1", "kind": "Service",
			"metadata": metadata("backstage", "service-uid")}},
		"v1/Pod(related)": []any{
			map[string]any{"apiVersion": "v1", "kind": "PodList",
				"metadata": map[string]any{"resourceVersion": "100"},
				"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod",
					"metadata": podMetadata()}},
			},
		}}
	return release, live
}

// metadata gives each fixture a stable identity and concurrency version.
func metadata(name, uid string) map[string]any {
	return map[string]any{"name": name, "namespace": "backstage", "uid": uid, "resourceVersion": "12"}
}

// podMetadata supplies the reviewed portal selector without selecting database pods.
func podMetadata() map[string]any {
	m := metadata("backstage-pod", "pod-uid")
	m["labels"] = portalLabels()
	return m
}

// portalLabels is the exact known selector; near matches must not pass.
func portalLabels() map[string]any {
	return map[string]any{"app.kubernetes.io/name": "backstage", "app.kubernetes.io/instance": "backstage", "app.kubernetes.io/component": "backstage"}
}

// encode fails the test when its locally constructed fixture cannot be serialized.
func encode(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestInstalledStatelessReleaseAssessment proves the positive control with retained data.
func TestInstalledStatelessReleaseAssessment(t *testing.T) {
	release, live := fixture()
	got := assess(encode(t, release), encode(t, live))
	if got.Code != "ASSESSED_STATELESS" || got.Objects != 2 {
		t.Fatalf("installed stateless release not assessed: %+v", got)
	}
	// A retained CNPG-owned claim is not part of Helm's stateless footprint.
	items := live["items"].([]any)
	items[3].(map[string]any)["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "backstage-db", "uid": "database-uid"}}
	if got := assess(encode(t, release), encode(t, live)); got.Code != "ASSESSED_STATELESS" {
		t.Fatalf("retained database owner refused: %+v", got)
	}
}

// TestUnsafeOrIncompleteFootprintNeverPasses rejects one risk or missing proof at a time.
func TestUnsafeOrIncompleteFootprintNeverPasses(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"wrong release":    func(r, _ map[string]any) { r["name"] = "other" },
		"wrong namespace":  func(r, _ map[string]any) { r["namespace"] = "other" },
		"pending upgrade":  func(r, _ map[string]any) { r["info"] = map[string]any{"status": "pending-upgrade"} },
		"missing revision": func(r, _ map[string]any) { delete(r, "version") },
		"empty manifest":   func(r, _ map[string]any) { r["manifest"] = "" },
		"malformed YAML":   func(r, _ map[string]any) { r["manifest"] = "apiVersion: [" },
		"duplicate YAML": func(r, _ map[string]any) {
			r["manifest"] = strings.ReplaceAll(deploymentManifest, "kind: Service", "kind: Service\nkind: Deployment")
		},
		"duplicate object": func(r, _ map[string]any) { r["manifest"] = deploymentManifest + "---\n" + deploymentManifest },
		"different API group": func(r, _ map[string]any) {
			r["manifest"] = strings.ReplaceAll(deploymentManifest, "apps/v1", "other.invalid/v1")
		},
		"other namespace": func(r, _ map[string]any) {
			r["manifest"] = strings.ReplaceAll(deploymentManifest, "namespace: backstage", "namespace: other")
		},
		"storage object": func(r, _ map[string]any) {
			r["manifest"] = deploymentManifest + "---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: extra\n  namespace: backstage\n"
		},
		"database object": func(r, _ map[string]any) {
			r["manifest"] = deploymentManifest + "---\napiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: extra\n  namespace: backstage\n"
		},
		"stored delete hook": func(r, _ map[string]any) {
			r["hooks"] = []any{map[string]any{"events": []any{"pre-delete"}, "manifest": "DO_NOT_PRINT"}}
		},
		"hook annotation": func(r, _ map[string]any) {
			r["manifest"] = strings.Replace(deploymentManifest, "  name: backstage", "  annotations:\n    helm.sh/hook: pre-delete\n  name: backstage", 1)
		},
		"retained Helm object": func(r, _ map[string]any) {
			r["manifest"] = strings.Replace(deploymentManifest, "  name: backstage", "  annotations:\n    helm.sh/resource-policy: keep\n  name: backstage", 1)
		},
		"missing live deployment": func(_, l map[string]any) { l["items"] = l["items"].([]any)[1:] },
		"changed pod selector": func(_, l map[string]any) {
			item(l, 0)["spec"].(map[string]any)["selector"] = map[string]any{"matchLabels": map[string]any{"other": "label"}}
		},
		"missing pods": func(_, l map[string]any) {
			items := l["items"].([]any)
			l["items"] = []any{items[0], items[2], items[3]}
		},
		"missing retained claims": func(_, l map[string]any) { l["items"] = l["items"].([]any)[:3] },
		"missing database": func(_, l map[string]any) {
			items := l["items"].([]any)
			l["items"] = []any{items[0], items[1], items[3]}
		},
		"terminating claim": func(_, l map[string]any) {
			item(l, 3)["metadata"].(map[string]any)["deletionTimestamp"] = "2026-01-01T00:00:00Z"
		},
		"claim owned by removed pod": func(_, l map[string]any) {
			item(l, 3)["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"kind": "Pod", "uid": "pod-uid"}}
		},
		"wrong database owner UID": func(_, l map[string]any) {
			item(l, 3)["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "backstage-db", "uid": "different-uid"}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, l := fixture()
			change(r, l)
			if got := assess(encode(t, r), encode(t, l)); got.Code == "ASSESSED_STATELESS" {
				t.Fatalf("unsafe footprint passed: %+v", got)
			}
		})
	}
}

// protectedRunner emulates only the environment guards, never real credentials.
func protectedRunner(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", "devantler-tech/platform")
	t.Setenv("GITHUB_REF", "refs/heads/main")
}

// TestCLIIsBoundedReadOnlyAndDoesNotLeakEvidence binds the fixed reader and output boundary.
func TestCLIIsBoundedReadOnlyAndDoesNotLeakEvidence(t *testing.T) {
	protectedRunner(t)
	r, l := fixture()
	var commands []string
	execute := func(_ context.Context, program string, args ...string) ([]byte, error) {
		commands = append(commands, program+" "+strings.Join(args, " "))
		if program == "helm" {
			return encode(t, r), nil
		}
		return encode(t, l), nil
	}
	var output bytes.Buffer
	if code := run([]string{"--live"}, nil, &output, execute); code != 0 {
		t.Fatalf("CLI failed: %d %s", code, &output)
	}
	expected := []string{
		"helm status backstage --namespace=backstage --kube-context=admin@prod --output=json",
		"kubectl --context=admin@prod --namespace=backstage get deployment,pod,persistentvolumeclaim,clusters.postgresql.cnpg.io --output=json --request-timeout=30s",
	}
	if !reflect.DeepEqual(commands, append(expected, expected...)) {
		t.Fatalf("unexpected command boundary: %v", commands)
	}
	if output.String() != "PORTAL-FOOTPRINT=ASSESSED_STATELESS objects=2 dataRetirement=UNASSESSED mutation=NONE\n" {
		t.Fatalf("unsafe or ambiguous public output: %s", &output)
	}
	// Neither a transport error nor a malformed sensitive body is published.
	output.Reset()
	if run([]string{"--live"}, nil, &output, func(context.Context, string, ...string) ([]byte, error) { return nil, fmt.Errorf("DO_NOT_PRINT") }) != 2 || strings.Contains(output.String(), "DO_NOT_PRINT") {
		t.Fatal("transport failure escaped")
	}
	output.Reset()
	if run([]string{"--live"}, nil, &output, func(context.Context, string, ...string) ([]byte, error) { return []byte("DO_NOT_PRINT"), nil }) != 2 || strings.Contains(output.String(), "DO_NOT_PRINT") {
		t.Fatal("raw body escaped")
	}
}

// TestChangedSnapshotAndNonMainNeverPass prevents stale or unreviewed assessment.
func TestChangedSnapshotAndNonMainNeverPass(t *testing.T) {
	protectedRunner(t)
	r, l := fixture()
	calls := 0
	execute := func(_ context.Context, program string, _ ...string) ([]byte, error) {
		calls++
		if program == "helm" {
			if calls > 2 {
				r["version"] = 5
			}
			return encode(t, r), nil
		}
		return encode(t, l), nil
	}
	var output bytes.Buffer
	if run([]string{"--live"}, nil, &output, execute) != 2 || !strings.Contains(output.String(), "HOLD_CHANGED_SNAPSHOT") {
		t.Fatal("changing installed revision passed")
	}
	for _, ref := range []string{"refs/pull/1/merge", "refs/heads/gh-readonly-queue/main/pr-1", "refs/heads/feature"} {
		t.Setenv("GITHUB_REF", ref)
		calls = 0
		if run([]string{"--live"}, nil, &output, execute) != 2 || calls != 0 {
			t.Fatal("non-main accessed production")
		}
	}
	for _, args := range [][]string{nil, {"--apply"}, {"--live", "other-target"}} {
		calls = 0
		if run(args, nil, &output, execute) != 2 || calls != 0 {
			t.Fatal("unsupported mutation or target accepted")
		}
	}
}

// TestOfflineCLIRejectsDuplicateInputKeys prevents ambiguous wrapper evidence.
func TestOfflineCLIRejectsDuplicateInputKeys(t *testing.T) {
	r, l := fixture()
	release, live := encode(t, r), encode(t, l)
	var output bytes.Buffer
	input := fmt.Sprintf(`{"Release":{},"Release":%s,"Live":%s}`, release, live)
	if run([]string{"--stdin"}, strings.NewReader(input), &output, nil) != 2 {
		t.Fatal("duplicate release silently overwritten")
	}
	output.Reset()
	input = fmt.Sprintf(`{"Release":%s,"Live":%s}`, release, live)
	if run([]string{"--stdin"}, strings.NewReader(input), &output, nil) != 0 {
		t.Fatalf("offline consumer failed: %s", &output)
	}
	if strings.Contains(output.String(), "DO_NOT_PRINT") {
		t.Fatal("sensitive evidence leaked")
	}
}

// TestResponseLimit proves bounded command capture and withheld raw error details.
func TestResponseLimit(t *testing.T) {
	var output boundedOutput
	if _, err := output.Write(bytes.Repeat([]byte("x"), maxResponse)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("x")); err == nil {
		t.Fatal("oversized command body accepted")
	}
	if assess(bytes.Repeat([]byte("x"), maxResponse+1), nil).Code == "ASSESSED_STATELESS" {
		t.Fatal("oversized evidence accepted")
	}
}

type failedOutput struct{}

// Write models a failed assessment sink without accepting any bytes.
func (failedOutput) Write([]byte) (int, error) { return 0, fmt.Errorf("output unavailable") }

// TestUnpublishedAssessmentCannotReportSuccess rejects loss of the final receipt.
func TestUnpublishedAssessmentCannotReportSuccess(t *testing.T) {
	r, l := fixture()
	input := encode(t, map[string]any{"Release": r, "Live": l})
	if run([]string{"--stdin"}, bytes.NewReader(input), failedOutput{}, nil) == 0 {
		t.Fatal("lost receipt reported success")
	}
}

// TestNativeOfflineConsumer exercises the actual CLI without cluster access.
func TestNativeOfflineConsumer(t *testing.T) {
	r, l := fixture()
	input := encode(t, map[string]any{"Release": r, "Live": l})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "--stdin")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "PORTAL-FOOTPRINT=ASSESSED_STATELESS objects=2 dataRetirement=UNASSESSED mutation=NONE\n" {
		t.Fatalf("native offline CLI failed or disclosed raw evidence: %v", err)
	}
}

// TestProtectedWorkflowUsesOnlyReviewedReadSurface binds main, confirmation and tool trust.
func TestProtectedWorkflowUsesOnlyReviewedReadSurface(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "assess-backstage-retirement.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(workflow["on"], map[string]any{"workflow_dispatch": map[string]any{"inputs": map[string]any{"confirmation": map[string]any{"description": "Type ASSESS BACKSTAGE FOOTPRINT for this read-only assessment", "required": true, "type": "string"}}}}) || len(object(workflow["permissions"])) != 0 {
		t.Fatal("unexpected workflow trigger or permissions")
	}
	if !reflect.DeepEqual(workflow["concurrency"], map[string]any{"group": "prod-deploy", "cancel-in-progress": false, "queue": "max"}) {
		t.Fatal("production lock changed")
	}
	jobs := object(workflow["jobs"])
	job := object(jobs["assess"])
	if len(jobs) != 1 || job["environment"] != "prod" || !reflect.DeepEqual(job["permissions"], map[string]any{"contents": "read"}) {
		t.Fatal("protected read boundary changed")
	}
	steps := job["steps"].([]any)
	installer := object(steps[3])
	if !reflect.DeepEqual(installer["env"], map[string]any{"HELM_VERSION": "v4.2.0", "HELM_SHA256": "97dbeb971be4ac4b27e3839976d9564c0fb35c6f3b1da89dd1e292d236af4096"}) {
		t.Fatal("Helm reader lacks the reviewed independent checksum")
	}
	if strings.Contains(text(installer["run"]), "curl --fail --location --retry 3 --output \"$archive.sha256sum\"") {
		t.Fatal("downloaded checksum substituted for reviewed trust anchor")
	}
	if !strings.Contains(text(installer["run"]), `printf '%s  %s\n' "$HELM_SHA256" "$archive" | sha256sum --check -`) {
		t.Fatal("reviewed checksum not used to verify the archive")
	}
	gate := text(object(steps[0])["run"])
	if object(object(steps[0])["env"])["CONFIRMATION"] != "${{ inputs.confirmation }}" {
		t.Fatal("typed confirmation not passed through the safe environment boundary")
	}
	for _, check := range []string{`[[ "$CONFIRMATION" == 'ASSESS BACKSTAGE FOOTPRINT' ]]`, `[[ "$RUN_REF" == refs/heads/main ]]`, `[[ "$RUN_SHA" =~ ^[0-9a-f]{40}$ ]]`, `[[ "$(gh api repos/devantler-tech/platform/branches/main --jq .commit.sha)" == "$RUN_SHA" ]]`} {
		if !strings.Contains(gate, check) {
			t.Fatal("current-main gate missing before credentials")
		}
	}
	last := object(steps[len(steps)-1])
	if last["run"] != "go run ./scripts/assess-backstage-retirement --live" || !reflect.DeepEqual(last["env"], map[string]any{"HELM_DRIVER": "secret"}) {
		t.Fatal("reviewed reader changed")
	}
}

// item selects a locally constructed fixture, never an untrusted production object.
func item(l map[string]any, index int) map[string]any {
	return l["items"].([]any)[index].(map[string]any)
}

// TestLiveAndStoredWorkloadTerminationRisks rejects persistent mounts and stop hooks.
func TestLiveAndStoredWorkloadTerminationRisks(t *testing.T) {
	for _, source := range []string{"stored", "deployment", "pod"} {
		for _, risk := range []string{"persistentVolumeClaim", "ephemeral", "csi", "hostPath", "preStop", "init-preStop"} {
			t.Run(source+"/"+risk, func(t *testing.T) {
				r, l := fixture()
				var spec map[string]any
				if source == "stored" {
					if strings.Contains(risk, "preStop") {
						r["manifest"] = strings.Replace(deploymentManifest, "          image:", "          lifecycle:\n            preStop:\n              exec:\n                command: [DO_NOT_PRINT]\n          image:", 1)
					} else {
						r["manifest"] = strings.Replace(deploymentManifest, "emptyDir: {}", risk+": {}", 1)
					}
				} else {
					spec = item(l, 1)["spec"].(map[string]any)
					if source == "deployment" {
						spec = item(l, 0)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
					}
					if strings.Contains(risk, "preStop") {
						key := "containers"
						if risk == "init-preStop" {
							key = "initContainers"
						}
						spec[key] = []any{map[string]any{"name": "backstage", "lifecycle": map[string]any{"preStop": map[string]any{"exec": map[string]any{"command": []any{"DO_NOT_PRINT"}}}}}}
					} else {
						spec["volumes"] = []any{map[string]any{"name": "extra", risk: map[string]any{}}}
					}
				}
				if got := assess(encode(t, r), encode(t, l)); got.Code == "ASSESSED_STATELESS" {
					t.Fatalf("termination risk passed: %+v", got)
				}
			})
		}
	}
}
