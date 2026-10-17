package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v5"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testRevision = "latest@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testProvenance = `[{"verificationResult":{"signature":{"certificate":{"issuer":"https://token.actions.githubusercontent.com","subjectAlternativeName":"https://github.com/devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/gh-readonly-queue/main/pr-12-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sourceRepositoryURI":"https://github.com/devantler-tech/platform","sourceRepositoryDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sourceRepositoryRef":"refs/heads/gh-readonly-queue/main/pr-12-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","runInvocationURI":"https://github.com/devantler-tech/platform/actions/runs/456/attempts/1"}},"statement":{"predicateType":"https://slsa.dev/provenance/v1","subject":[{"name":"ghcr.io/devantler-tech/platform/manifests","digest":{"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}],"predicate":{"run":"https://github.com/devantler-tech/platform/actions/runs/456/attempts/1","source":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}]`

// testEnvironment supplies only synthetic first-attempt protected invocation facts.
func testEnvironment() map[string]string {
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/protect-backstage-storage.yaml@refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_SHA": testSHA, "GITHUB_RUN_ID": "123", "PORTAL_STORAGE_OPERATION": "protect", "PORTAL_STORAGE_CONFIRM": "PROTECT BACKSTAGE STORAGE ONLY", "PORTAL_STORAGE_REVISION": testRevision, "PORTAL_STORAGE_DEPLOYMENT_RUN": "456"}
}

type storageAPI struct {
	t                                            *testing.T
	ns, db, claims, volumes, pods                map[string]any
	acceptedRun                                  map[string]any
	provenanceBody                               []byte
	provenanceFailure                            bool
	verifications                                int
	patches, pauses, collections                 int
	commands                                     []string
	onRead                                       func(*storageAPI, string)
	onPatch                                      func(*storageAPI)
	readFailure                                  string
	staleSource, missingDeployment, patchFailure bool
}

// unmarshal decodes hand-written test objects without using production acceptance logic.
func unmarshal(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// mchild selects an expected synthetic nested object.
func mchild(m map[string]any, k string) map[string]any { return m[k].(map[string]any) }

// mitem selects an expected synthetic list item.
func mitem(m map[string]any, n int) map[string]any { return m["items"].([]any)[n].(map[string]any) }

// storageFixture supplies an independent two-instance database and exact signed publication witness.
func storageFixture(t *testing.T) *storageAPI {
	t.Helper()
	a := &storageAPI{t: t}
	a.acceptedRun = unmarshal(t, `{"id":456,"run_attempt":1,"head_branch":"gh-readonly-queue/main/pr-12-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","path":".github/workflows/ci.yaml","event":"merge_group","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"completed","conclusion":"success","repository":{"full_name":"devantler-tech/platform"}}`)
	a.ns = unmarshal(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"backstage","uid":"namespace","resourceVersion":"1","annotations":{"kustomize.toolkit.fluxcd.io/prune":"disabled"}},"spec":{"finalizers":["kubernetes"]},"status":{"phase":"Active"}}`)
	a.db = unmarshal(t, `{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"namespace":"backstage","name":"backstage-db","uid":"database","resourceVersion":"2","generation":1},"spec":{"instances":2,"storage":{"storageClass":"longhorn-wffc","size":"1Gi"}},"status":{"readyInstances":2,"currentPrimary":"backstage-db-4","targetPrimary":"backstage-db-4"}}`)
	a.claims = unmarshal(t, `{"apiVersion":"v1","kind":"PersistentVolumeClaimList","metadata":{"resourceVersion":"100"},"items":[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"backstage-db-3","namespace":"backstage","uid":"claim-3","resourceVersion":"101","ownerReferences":[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","name":"backstage-db","uid":"database","controller":true}]},"spec":{"volumeName":"pv-3","storageClassName":"longhorn-wffc","resources":{"requests":{"storage":"1Gi"}}},"status":{"phase":"Bound"}},{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"backstage-db-4","namespace":"backstage","uid":"claim-4","resourceVersion":"102","ownerReferences":[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","name":"backstage-db","uid":"database","controller":true}]},"spec":{"volumeName":"pv-4","storageClassName":"longhorn-wffc","resources":{"requests":{"storage":"1Gi"}}},"status":{"phase":"Bound"}}]}`)
	a.volumes = unmarshal(t, `{"apiVersion":"v1","kind":"PersistentVolumeList","metadata":{"resourceVersion":"200"},"items":[{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv-3","uid":"volume-3","resourceVersion":"201","finalizers":["kubernetes.io/pv-protection"]},"spec":{"persistentVolumeReclaimPolicy":"Delete","storageClassName":"longhorn-wffc","claimRef":{"apiVersion":"v1","kind":"PersistentVolumeClaim","namespace":"backstage","name":"backstage-db-3","uid":"claim-3"},"capacity":{"storage":"1Gi"},"csi":{"driver":"driver.longhorn.io","volumeHandle":"disk-3"}},"status":{"phase":"Bound"}},{"apiVersion":"v1","kind":"PersistentVolume","metadata":{"name":"pv-4","uid":"volume-4","resourceVersion":"202"},"spec":{"persistentVolumeReclaimPolicy":"Delete","storageClassName":"longhorn-wffc","claimRef":{"apiVersion":"v1","kind":"PersistentVolumeClaim","namespace":"backstage","name":"backstage-db-4","uid":"claim-4"},"capacity":{"storage":"1Gi"},"csi":{"driver":"driver.longhorn.io","volumeHandle":"disk-4"}},"status":{"phase":"Bound"}}]}`)
	a.pods = unmarshal(t, `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"300"},"items":[{"apiVersion":"v1","kind":"Pod","metadata":{"namespace":"backstage","name":"backstage-db-3","uid":"pod-3","resourceVersion":"301","labels":{"cnpg.io/cluster":"backstage-db"},"ownerReferences":[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","name":"backstage-db","uid":"database","controller":true}]},"spec":{"containers":[{"name":"postgres"}],"volumes":[{"name":"pgdata","persistentVolumeClaim":{"claimName":"backstage-db-3"}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},{"apiVersion":"v1","kind":"Pod","metadata":{"namespace":"backstage","name":"backstage-db-4","uid":"pod-4","resourceVersion":"302","labels":{"cnpg.io/cluster":"backstage-db"},"ownerReferences":[{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","name":"backstage-db","uid":"database","controller":true}]},"spec":{"containers":[{"name":"postgres"}],"volumes":[{"name":"pgdata","persistentVolumeClaim":{"claimName":"backstage-db-4"}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`)
	return a
}

// execute simulates external reads and applies the real emitted JSON Patch to mutable synthetic API objects.
func (a *storageAPI) execute(_ context.Context, name string, args ...string) ([]byte, error) {
	s := strings.Join(args, " ")
	a.commands = append(a.commands, name+" "+s)
	if a.readFailure != "" && strings.Contains(s, a.readFailure) {
		return []byte(`{"plausible":"response"}`), errors.New("sensitive failed read")
	}
	marshal := func(v any) ([]byte, error) { return json.Marshal(v) }
	if name == "bash" && reflect.DeepEqual(args, []string{"scripts/verify-prod-recovery-source.sh", testSHA}) {
		if a.staleSource {
			return []byte("PROD_RECOVERY_SOURCE=HOLD"), nil
		}
		return []byte("PROD_RECOVERY_SOURCE=PASS\n"), nil
	}
	if name == "gh" {
		if len(args) > 1 && args[0] == "attestation" && args[1] == "verify" {
			a.verifications++
			identity := `^https://github\.com/devantler-tech/platform/\.github/workflows/ci\.yaml@refs/heads/gh-readonly-queue/main/pr-12-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa$`
			if a.acceptedRun["path"] == ".github/workflows/cd.yaml" {
				identity = `^https://github\.com/devantler-tech/platform/\.github/workflows/cd\.yaml@refs/heads/main$`
			}
			want := []string{"attestation", "verify", "oci://ghcr.io/devantler-tech/platform/manifests@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "--bundle-from-oci", "--repo", "devantler-tech/platform", "--predicate-type", "https://slsa.dev/provenance/v1", "--source-digest", testSHA, "--cert-identity-regex", identity, "--format", "json"}
			if !reflect.DeepEqual(args, want) {
				a.t.Fatalf("provenance verification lost exact artifact/source/workflow contract: %v", args)
			}
			b := a.provenanceBody
			if b == nil {
				b = []byte(testProvenance)
			}
			if a.provenanceFailure {
				return b, errors.New("private registry verification failure")
			}
			return b, nil
		}
		if !strings.HasPrefix(s, "api --hostname=github.com repos/devantler-tech/platform/") {
			a.t.Fatalf("unbounded API call %s", s)
		}
		switch args[len(args)-1] {
		case "repos/devantler-tech/platform/actions/runs/456":
			return marshal(a.acceptedRun)
		case "repos/devantler-tech/platform/deployments?sha=" + testSHA + "&environment=prod&per_page=100":
			if a.missingDeployment {
				return []byte("[]"), nil
			}
			return marshal([]any{map[string]any{"id": 789, "sha": testSHA, "environment": "prod"}})
		case "repos/devantler-tech/platform/deployments/789/statuses?per_page=100":
			return marshal([]any{map[string]any{"state": "success", "environment": "prod", "log_url": "https://github.com/devantler-tech/platform/actions/runs/456/job/987"}})
		}
	}
	if name != "kubectl" || len(args) < 3 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=30s" {
		a.t.Fatalf("unexpected command %s %s", name, s)
	}
	if a.onRead != nil && !strings.Contains(s, "patch persistentvolume") {
		a.onRead(a, s)
	}
	switch {
	case s == "--context=admin@prod --request-timeout=30s get namespace backstage --output=json":
		a.collections++
		return marshal(a.ns)
	case s == "--context=admin@prod --request-timeout=30s --namespace=backstage get clusters.postgresql.cnpg.io backstage-db --output=json":
		return marshal(a.db)
	case s == "--context=admin@prod --request-timeout=30s get --raw=/api/v1/namespaces/backstage/persistentvolumeclaims":
		return marshal(a.claims)
	case s == "--context=admin@prod --request-timeout=30s get --raw=/api/v1/persistentvolumes":
		return marshal(a.volumes)
	case s == "--context=admin@prod --request-timeout=30s get --raw=/api/v1/namespaces/backstage/pods":
		return marshal(a.pods)
	case strings.Contains(s, "get ocirepository flux-system"):
		return marshal(unmarshal(a.t, `{"apiVersion":"source.toolkit.fluxcd.io/v1","kind":"OCIRepository","metadata":{"namespace":"flux-system","name":"flux-system","uid":"source","resourceVersion":"10","generation":1},"spec":{},"status":{"observedGeneration":1,"artifact":{"revision":"`+testRevision+`"},"conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}`))
	case strings.Contains(s, "get kustomization "):
		for _, n := range []string{"flux-system", "bootstrap", "infrastructure-controllers", "infrastructure", "apps"} {
			if strings.Contains(s, "get kustomization "+n+" --output=json") {
				return marshal(unmarshal(a.t, `{"apiVersion":"kustomize.toolkit.fluxcd.io/v1","kind":"Kustomization","metadata":{"namespace":"flux-system","name":"`+n+`","uid":"layer-`+n+`","resourceVersion":"10","generation":1},"spec":{"sourceRef":{"kind":"OCIRepository","name":"flux-system"}},"status":{"observedGeneration":1,"lastAppliedRevision":"`+testRevision+`","conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}`))
			}
		}
	case len(args) == 10 && args[2] == "patch" && args[3] == "persistentvolume" && args[5] == "--type=json" && args[6] == "--patch" && args[8] == "--output=json" && args[9] == "--field-manager=portal-storage-protection":
		a.patches++
		if a.onPatch != nil {
			a.onPatch(a)
		}
		if a.patchFailure {
			return nil, errors.New("sensitive transport diagnostic")
		}
		for _, raw := range a.volumes["items"].([]any) {
			v := raw.(map[string]any)
			if mchild(v, "metadata")["name"] != args[4] {
				continue
			}
			b, err := marshal(v)
			if err != nil {
				return nil, err
			}
			p, err := jsonpatch.DecodePatch([]byte(args[7]))
			if err != nil {
				return nil, err
			}
			changed, err := p.Apply(b)
			if err != nil {
				return nil, err
			}
			next := unmarshal(a.t, string(changed))
			mchild(next, "metadata")["resourceVersion"] = fmt.Sprint(mchild(v, "metadata")["resourceVersion"]) + "1"
			for k := range v {
				delete(v, k)
			}
			for k, val := range next {
				v[k] = val
			}
			return marshal(v)
		}
	}
	a.t.Fatalf("unexpected command %s %s", name, s)
	return nil, errors.New("unexpected command")
}

// wait verifies the requested safety interval without introducing wall-clock sleeps.
func (a *storageAPI) wait(_ context.Context, d time.Duration) error {
	if d < 5*time.Second {
		a.t.Fatal("safety samples not separated")
	}
	a.pauses++
	return nil
}

// executeStorage exercises the production run entrypoint and captures its caller-visible result.
func executeStorage(t *testing.T, a *storageAPI, env map[string]string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), []string{"--execute"}, func(k string) string { return env[k] }, a.execute, a.wait, &out)
	return out.String(), err
}

// TestStorageProtectionChangesOnlyBoundVolumeRetention checks both real patch effects and preservation of non-volume objects.
func TestStorageProtectionChangesOnlyBoundVolumeRetention(t *testing.T) {
	a := storageFixture(t)
	beforeClaims, _ := json.Marshal(a.claims)
	beforeDB, _ := json.Marshal(a.db)
	beforeNS, _ := json.Marshal(a.ns)
	out, err := executeStorage(t, a, testEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if a.patches != 2 || a.pauses != 2 || a.collections < 6 {
		t.Fatalf("patches=%d pauses=%d reads=%d; want two writes with separated full pre/post reads", a.patches, a.pauses, a.collections)
	}
	for i := 0; i < 2; i++ {
		v := mitem(a.volumes, i)
		if mchild(v, "spec")["persistentVolumeReclaimPolicy"] != "Retain" || mchild(mchild(v, "spec"), "csi")["volumeHandle"] != fmt.Sprintf("disk-%d", i+3) {
			t.Fatal("physical volume identity not preserved")
		}
	}
	for _, pair := range [][2][]byte{{beforeClaims, mustJSON(t, a.claims)}, {beforeDB, mustJSON(t, a.db)}, {beforeNS, mustJSON(t, a.ns)}} {
		if !bytes.Equal(pair[0], pair[1]) {
			t.Fatal("non-volume object changed")
		}
	}
	if !strings.Contains(out, `"changed":2`) || strings.Contains(out, "disk-") || strings.Contains(out, "volume-") {
		t.Fatalf("wrong or sensitive summary: %s", out)
	}
}

// mustJSON encodes synthetic evidence for transport fixtures and fails the test on encoding errors.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStorageProtectionRetainIsNoOp requires already-retained volumes to complete without any patch.
func TestStorageProtectionRetainIsNoOp(t *testing.T) {
	a := storageFixture(t)
	for i := 0; i < 2; i++ {
		mchild(mitem(a.volumes, i), "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	}
	out, err := executeStorage(t, a, testEnvironment())
	if err != nil || a.patches != 0 || !strings.Contains(out, `"changed":0`) {
		t.Fatalf("no-op got writes=%d error=%v output=%s", a.patches, err, out)
	}
}

// TestStorageProtectionRefusesUnboundInvocationBeforeAnyRead denies wrong caller, ref, attempt, confirmation and source facts before tool access.
func TestStorageProtectionRefusesUnboundInvocationBeforeAnyRead(t *testing.T) {
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_WORKFLOW_REF", "GITHUB_RUN_ATTEMPT", "GITHUB_SHA", "GITHUB_RUN_ID", "PORTAL_STORAGE_CONFIRM", "PORTAL_STORAGE_REVISION", "PORTAL_STORAGE_DEPLOYMENT_RUN"} {
		t.Run(key, func(t *testing.T) {
			env := testEnvironment()
			env[key] = "wrong"
			a := storageFixture(t)
			if _, err := executeStorage(t, a, env); err == nil || len(a.commands) != 0 {
				t.Fatal("unbound protected invocation made a read or succeeded")
			}
		})
	}
}

// TestStorageProtectionRefusesUnsafeInitialEvidence keeps incomplete ownership, availability and deployment evidence from authorizing writes.
func TestStorageProtectionRefusesUnsafeInitialEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*storageAPI)
	}{
		{"journal exists", func(a *storageAPI) {
			mchild(mchild(a.ns, "metadata"), "annotations")["platform.devantler.tech/backstage-retirement"] = "owned-or-foreign"
		}},
		{"database not redundant", func(a *storageAPI) { mchild(a.db, "status")["readyInstances"] = float64(1) }},
		{"database failover", func(a *storageAPI) { mchild(a.db, "status")["targetPrimary"] = "backstage-db-3" }},
		{"missing pod", func(a *storageAPI) { a.pods["items"] = a.pods["items"].([]any)[:1] }},
		{"pod unready", func(a *storageAPI) {
			mchild(mitem(a.pods, 0), "status")["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
		}},
		{"pod wrong owner", func(a *storageAPI) {
			mchild(mitem(a.pods, 0), "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "other"
		}},
		{"missing list RV", func(a *storageAPI) { mchild(a.claims, "metadata")["resourceVersion"] = "" }},
		{"duplicate handles", func(a *storageAPI) { mchild(mchild(mitem(a.volumes, 1), "spec"), "csi")["volumeHandle"] = "disk-3" }},
		{"wrong CSI driver", func(a *storageAPI) { mchild(mchild(mitem(a.volumes, 1), "spec"), "csi")["driver"] = "other" }},
		{"wrong pod claim", func(a *storageAPI) {
			mchild(mitem(a.pods, 0), "spec")["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"] = map[string]any{"claimName": "backstage-db-4"}
		}},
		{"missing accepted deployment", func(a *storageAPI) { a.missingDeployment = true }},
		{"main advanced", func(a *storageAPI) { a.staleSource = true }},
		{"bootstrap unavailable", func(a *storageAPI) { a.readFailure = "get kustomization bootstrap" }},
		{"accepted API failed with body", func(a *storageAPI) { a.readFailure = "actions/runs/456" }},
		{"volume name cannot be a command option", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["name"] = "--bad-option"
			mchild(mitem(a.claims, 0), "spec")["volumeName"] = "--bad-option"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			tc.mutate(a)
			if _, err := executeStorage(t, a, testEnvironment()); err == nil || a.patches != 0 {
				t.Fatal("unsafe initial state mutated or accepted")
			}
		})
	}
}

type failedResultWriter struct{ short bool }

// Write injects terminal-output failure after the real protection effects have completed.
func (w failedResultWriter) Write(b []byte) (int, error) {
	if w.short {
		return len(b) - 1, nil
	}
	return 0, errors.New("closed output")
}

// TestStorageProtectionNeverSucceedsWithMissingTerminalReceipt rejects failed or short output without reversing established protection.
func TestStorageProtectionNeverSucceedsWithMissingTerminalReceipt(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			a := storageFixture(t)
			env := testEnvironment()
			err := run(context.Background(), []string{"--execute"}, func(k string) string { return env[k] }, a.execute, a.wait, failedResultWriter{short: short})
			if err == nil {
				t.Fatal("incomplete terminal receipt reported success")
			}
			if a.patches != 2 {
				t.Fatal("receipt error lost actual protection outcome")
			}
		})
	}
}

// TestStorageProtectionBindsSourceAndAcceptedDeploymentEvidence rejects foreign source joins and forbids this maintenance job from serving as its own publisher.
func TestStorageProtectionBindsSourceAndAcceptedDeploymentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		mutate        func(map[string]any)
		failed        bool
	}{
		{"foreign explicit layer source namespace", "get kustomization apps", func(r map[string]any) { mchild(mchild(r, "spec"), "sourceRef")["namespace"] = "other" }, false},
		{"unaccepted workflow path", "actions/runs/456", func(r map[string]any) { r["path"] = ".github/workflows/protect-backstage-storage.yaml" }, false},
		{"unaccepted workflow event", "actions/runs/456", func(r map[string]any) { r["event"] = "pull_request" }, false},
		{"unaccepted source sha", "actions/runs/456", func(r map[string]any) { r["head_sha"] = "cccccccccccccccccccccccccccccccccccccccc" }, false},
		{"wrong run identity", "actions/runs/456", func(r map[string]any) { r["id"] = float64(999) }, false},
		{"failed read with genuine-looking successful run body", "actions/runs/456", func(r map[string]any) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			env := testEnvironment()
			var out bytes.Buffer
			wrapped := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				b, err := a.execute(ctx, name, args...)
				if err != nil || !strings.Contains(strings.Join(args, " "), tc.command) {
					return b, err
				}
				r := unmarshal(t, string(b))
				tc.mutate(r)
				b = mustJSON(t, r)
				if tc.failed {
					return b, errors.New("failed transport with private evidence")
				}
				return b, nil
			}
			err := run(context.Background(), []string{"--execute"}, func(k string) string { return env[k] }, wrapped, a.wait, &out)
			if err == nil || a.patches != 0 || out.Len() != 0 {
				t.Fatalf("unbound source/deployment accepted: writes=%d error=%v", a.patches, err)
			}
		})
	}
}

// TestStorageProtectionStopsOnLateDriftAndAmbiguousPatch exercises actual CAS rejection, no retries, late journals and partially completed protection.
func TestStorageProtectionStopsOnLateDriftAndAmbiguousPatch(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(*storageAPI)
		wantPatches int
	}{
		{"claim spec drift before patch", func(a *storageAPI) {
			a.onRead = func(a *storageAPI, s string) {
				if a.collections == 2 && strings.Contains(s, "persistentvolumeclaims") {
					mchild(mitem(a.claims, 0), "spec")["volumeName"] = "other"
				}
			}
		}, 0},
		{"main advanced before patch", func(a *storageAPI) {
			a.onRead = func(a *storageAPI, s string) {
				if a.collections == 3 && strings.Contains(s, "/pods") {
					a.staleSource = true
				}
			}
		}, 0},
		{"CAS rejects changed storage handle", func(a *storageAPI) {
			a.onPatch = func(a *storageAPI) {
				mchild(mchild(mitem(a.volumes, 0), "spec"), "csi")["volumeHandle"] = "changed-underfoot"
			}
		}, 1},
		{"CAS rejects resource version", func(a *storageAPI) {
			a.onPatch = func(a *storageAPI) { mchild(mitem(a.volumes, 0), "metadata")["resourceVersion"] = "999" }
		}, 1},
		{"ambiguous transport stops without retry", func(a *storageAPI) { a.patchFailure = true }, 1},
		{"CAS rejects replaced PV UID", func(a *storageAPI) {
			a.onPatch = func(a *storageAPI) { mchild(mitem(a.volumes, 0), "metadata")["uid"] = "replacement" }
		}, 1},
		{"final separated read refuses new journal", func(a *storageAPI) {
			a.onRead = func(a *storageAPI, s string) {
				if a.pauses == 2 && strings.Contains(s, "get namespace backstage") {
					mchild(mchild(a.ns, "metadata"), "annotations")[journal] = "new-journal"
				}
			}
		}, 2},
		{"second write must not use stale first snapshot", func(a *storageAPI) {
			a.onRead = func(a *storageAPI, s string) {
				if a.patches == 1 && strings.Contains(s, "persistentvolumeclaims") {
					mchild(mitem(a.claims, 1), "metadata")["uid"] = "replacement"
				}
			}
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			tc.setup(a)
			out, err := executeStorage(t, a, testEnvironment())
			if err == nil || a.patches != tc.wantPatches || out != "" {
				t.Fatalf("writes=%d want=%d error=%v output=%s", a.patches, tc.wantPatches, err, out)
			}
		})
	}
}
