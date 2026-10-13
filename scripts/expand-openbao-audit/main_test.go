package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fixture struct {
	t               *testing.T
	claims, volumes map[string]map[string]any
	patched         []string
	receipt         bool
	mode            string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/expand-openbao-audit.yaml@refs/heads/main")
	t.Setenv("GITHUB_REPOSITORY", "devantler-tech/platform")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("AUDIT_CAPACITY_CONFIRM", "EXPAND OPENBAO AUDIT ONLY TO 20GI")
	t.Setenv("AUDIT_CAPACITY_REVISION", "latest@sha256:"+strings.Repeat("b", 64))
	t.Setenv("AUDIT_CAPACITY_DEPLOYMENT_RUN", "100")
	f := &fixture{t: t, claims: map[string]map[string]any{}, volumes: map[string]map[string]any{}}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("audit-openbao-%d", i)
		pv := fmt.Sprintf("volume-%d", i)
		f.claims[name] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": name, "namespace": "openbao", "uid": name + "-uid", "resourceVersion": "1", "labels": map[string]any{"app.kubernetes.io/name": "openbao"}}, "spec": map[string]any{"storageClassName": "hcloud", "volumeName": pv, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}}, "status": map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "10Gi"}}}
		f.volumes[pv] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": pv, "uid": pv + "-uid", "resourceVersion": "1"}, "spec": map[string]any{"storageClassName": "hcloud", "capacity": map[string]any{"storage": "10Gi"}, "csi": map[string]any{"driver": "csi.hetzner.cloud", "volumeHandle": "PRIVATE_HANDLE" + pv}, "claimRef": map[string]any{"namespace": "openbao", "name": name, "uid": name + "-uid"}}, "status": map[string]any{"phase": "Bound"}}
	}
	return f
}

func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func (f *fixture) execute(_ context.Context, p string, a ...string) ([]byte, error) {
	args := strings.Join(a, " ")
	if p == "sleep" {
		return nil, nil
	}
	if p == "bash" {
		return []byte("PROD_RECOVERY_SOURCE=PASS\n"), nil
	}
	if p == "gh" {
		if strings.Contains(args, "actions/runs/100") {
			return jsonBytes(f.t, map[string]any{"id": 100, "head_sha": strings.Repeat("a", 40), "status": "completed", "conclusion": "success", "path": ".github/workflows/ci.yaml", "event": "merge_group", "repository": map[string]any{"full_name": "devantler-tech/platform"}}), nil
		}
		if strings.Contains(args, "deployments?") {
			return jsonBytes(f.t, []any{map[string]any{"id": 7, "sha": strings.Repeat("a", 40), "environment": "prod"}}), nil
		}
		if strings.Contains(args, "deployments/7/statuses") {
			return jsonBytes(f.t, []any{map[string]any{"state": "success", "environment": "prod", "log_url": "https://github.com/devantler-tech/platform/actions/runs/100/job/123"}}), nil
		}
	}
	if p != "kubectl" {
		return nil, fmt.Errorf("unexpected program %s", p)
	}
	if strings.Contains(args, "exec openbao-") {
		sealed := f.mode == "sealed"
		return jsonBytes(f.t, map[string]any{"initialized": true, "sealed": sealed, "ha_enabled": true}), nil
	}
	if strings.Contains(args, "get statefulset openbao") {
		return jsonBytes(f.t, map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": "openbao", "namespace": "openbao", "uid": "sts-uid", "resourceVersion": "1", "generation": 1}, "spec": map[string]any{"replicas": 3}, "status": map[string]any{"observedGeneration": 1, "readyReplicas": 3, "currentReplicas": 3, "updatedReplicas": 3}}), nil
	}
	if strings.Contains(args, "get pod openbao-") {
		var name string
		for i, v := range a {
			if v == "pod" {
				name = a[i+1]
			}
		}
		return jsonBytes(f.t, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name, "namespace": "openbao", "uid": name + "-uid", "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "openbao", "uid": "sts-uid", "controller": true}}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "openbao", "volumeMounts": []any{map[string]any{"name": "audit", "mountPath": "/openbao/audit"}}}}, "volumes": []any{map[string]any{"name": "audit", "persistentVolumeClaim": map[string]any{"claimName": "audit-" + name}}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}), nil
	}
	if strings.Contains(args, "get storageclass hcloud") {
		return jsonBytes(f.t, map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": "hcloud", "uid": "class-uid", "resourceVersion": "1"}, "provisioner": "csi.hetzner.cloud", "allowVolumeExpansion": f.mode != "expansion disabled"}), nil
	}
	if strings.Contains(args, "get kustomization ") || strings.Contains(args, "get ocirepository flux-system") {
		name := "flux-system"
		kind := "OCIRepository"
		api := "source.toolkit.fluxcd.io/v1"
		for i, v := range a {
			if v == "kustomization" {
				name = a[i+1]
				kind = "Kustomization"
				api = "kustomize.toolkit.fluxcd.io/v1"
			}
		}
		return jsonBytes(f.t, map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": "flux-system", "uid": name + "-uid", "resourceVersion": "1", "generation": 1}, "status": map[string]any{"observedGeneration": 1, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": 1}}, "artifact": map[string]any{"revision": os.Getenv("AUDIT_CAPACITY_REVISION")}, "lastAppliedRevision": os.Getenv("AUDIT_CAPACITY_REVISION")}}), nil
	}
	if strings.Contains(args, "get persistentvolumeclaim ") {
		for i, v := range a {
			if v == "persistentvolumeclaim" {
				return jsonBytes(f.t, f.claims[a[i+1]]), nil
			}
		}
	}
	if strings.Contains(args, "get persistentvolume ") {
		for i, v := range a {
			if v == "persistentvolume" {
				return jsonBytes(f.t, f.volumes[a[i+1]]), nil
			}
		}
	}
	if strings.Contains(args, "get configmap ") {
		if f.receipt {
			return f.receiptBytes(), nil
		}
		return nil, nil
	}
	if strings.Contains(args, "create -f ") {
		f.receipt = true
		return f.receiptBytes(), nil
	}
	if strings.Contains(args, "patch persistentvolumeclaim ") {
		var name, path string
		for i, v := range a {
			if v == "persistentvolumeclaim" {
				name = a[i+1]
			}
			if strings.HasPrefix(v, "--patch-file=") {
				path = strings.TrimPrefix(v, "--patch-file=")
			}
		}
		b, e := os.ReadFile(path)
		if e != nil {
			f.t.Fatal(e)
		}
		var patch []map[string]any
		if json.Unmarshal(b, &patch) != nil {
			f.t.Fatal("bad patch")
		}
		want := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": name + "-uid"}, {"op": "test", "path": "/metadata/resourceVersion", "value": "1"}, {"op": "replace", "path": "/spec/resources/requests/storage", "value": "20Gi"}}
		if !reflect.DeepEqual(patch, want) {
			f.t.Fatalf("unbounded or unconditioned patch: %v", patch)
		}
		if f.mode == "conflict" {
			return nil, fmt.Errorf("PRIVATE conflict")
		}
		f.patched = append(f.patched, name)
		c := f.claims[name]
		c["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = "20Gi"
		c["status"].(map[string]any)["capacity"].(map[string]any)["storage"] = "20Gi"
		f.volumes[c["spec"].(map[string]any)["volumeName"].(string)]["spec"].(map[string]any)["capacity"].(map[string]any)["storage"] = "20Gi"
		if f.mode == "ambiguous" {
			return nil, fmt.Errorf("PRIVATE ambiguous result")
		}
		return jsonBytes(f.t, c), nil
	}
	return nil, fmt.Errorf("unexpected command %s", args)
}

func (f *fixture) receiptBytes() []byte {
	return jsonBytes(f.t, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "openbao-audit-expansion-42", "namespace": "flux-system", "uid": "receipt-uid", "resourceVersion": "1"}, "immutable": true, "data": map[string]any{"source": os.Getenv("GITHUB_SHA"), "revision": os.Getenv("AUDIT_CAPACITY_REVISION"), "run": "42", "target": "20Gi", "scope": "audit-only"}})
}

func TestAdditionalUncertainEvidenceStopsExpansion(t *testing.T) {
	for _, mode := range []string{"malformed resize conditions", "larger backing volume", "lost receipt data", "post-patch changed handle", "post-patch changed annotations", "different accepted source", "failed deployment", "wrong deployment run"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			if mode == "malformed resize conditions" {
				obj(f.claims["audit-openbao-0"]["status"])["conditions"] = "unknown"
			}
			if mode == "larger backing volume" {
				obj(obj(f.volumes["volume-0"]["spec"])["capacity"])["storage"] = "30Gi"
			}
			wrapped := func(ctx context.Context, p string, a ...string) ([]byte, error) {
				b, err := f.execute(ctx, p, a...)
				joined := strings.Join(a, " ")
				if err != nil {
					return b, err
				}
				if mode == "lost receipt data" && strings.Contains(joined, "create -f") {
					r, _ := decode(b)
					delete(r, "data")
					return jsonBytes(t, r), nil
				}
				if mode == "post-patch changed handle" && len(f.patched) > 0 && strings.Contains(joined, "get persistentvolume volume-0") {
					r, _ := decode(b)
					obj(obj(r["spec"])["csi"])["volumeHandle"] = "replacement"
					return jsonBytes(t, r), nil
				}
				if mode == "post-patch changed annotations" && len(f.patched) > 0 && strings.Contains(joined, "get persistentvolumeclaim audit-openbao-0") {
					r, _ := decode(b)
					obj(r["metadata"])["annotations"] = map[string]any{"changed": "true"}
					return jsonBytes(t, r), nil
				}
				if strings.Contains(joined, "actions/runs/100") && mode == "different accepted source" {
					r, _ := decode(b)
					r["head_sha"] = strings.Repeat("c", 40)
					return jsonBytes(t, r), nil
				}
				if strings.Contains(joined, "deployments/7/statuses") && (mode == "failed deployment" || mode == "wrong deployment run") {
					var ss []map[string]any
					_ = json.Unmarshal(b, &ss)
					if mode == "failed deployment" {
						ss[0]["state"] = "failure"
					} else {
						ss[0]["log_url"] = "https://github.com/devantler-tech/platform/actions/runs/101/job/123"
					}
					return jsonBytes(t, ss), nil
				}
				return b, err
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, wrapped, func(context.Context) bool { return true }); code != 2 || len(f.patched) > 1 || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("uncertain evidence accepted: %d %v %s", code, f.patched, &out)
			}
		})
	}
}

func TestCostMustRemainWithinCapBeforeEveryPatch(t *testing.T) {
	f := setup(t)
	calls := 0
	var out bytes.Buffer
	code := run([]string{"--execute"}, &out, f.execute, func(context.Context) bool { calls++; return calls < 3 })
	if code != 2 || len(f.patched) != 1 || !strings.Contains(out.String(), "HOLD_CAPACITY_MAY_HAVE_CHANGED") {
		t.Fatalf("changing price ignored: %d calls=%d patches=%v %s", code, calls, f.patched, &out)
	}
}

func TestAmbiguousPodIdentityCannotAuthorizeCapacity(t *testing.T) {
	for _, mode := range []string{"not controller", "duplicate readiness", "subpath mount", "wrong mount"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			wrapped := func(ctx context.Context, p string, a ...string) ([]byte, error) {
				b, err := f.execute(ctx, p, a...)
				if err != nil || !strings.Contains(strings.Join(a, " "), "get pod openbao-") {
					return b, err
				}
				r, _ := decode(b)
				switch mode {
				case "not controller":
					obj(array(field(r, "metadata", "ownerReferences"))[0])["controller"] = false
				case "duplicate readiness":
					obj(r["status"])["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}, map[string]any{"type": "Ready", "status": "False"}}
				case "subpath mount":
					obj(array(obj(array(field(r, "spec", "containers"))[0])["volumeMounts"])[0])["subPath"] = "elsewhere"
				case "wrong mount":
					obj(array(field(r, "spec", "volumes"))[0])["persistentVolumeClaim"] = map[string]any{"claimName": "data-openbao-0"}
				}
				return jsonBytes(t, r), nil
			}
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, wrapped, func(context.Context) bool { return true }) != 2 || f.receipt || len(f.patched) > 0 {
				t.Fatalf("ambiguous mount accepted: %s", &out)
			}
		})
	}
}

func TestExpandOnlyAuditClaimsPreservingData(t *testing.T) {
	f := setup(t)
	var out bytes.Buffer
	if code := run([]string{"--execute"}, &out, f.execute, func(context.Context) bool { return true }); code != 0 || len(f.patched) != 3 || !f.receipt {
		t.Fatalf("expansion not proven: code=%d patches=%v %s", code, f.patched, &out)
	}
	if out.String() != "OPENBAO_AUDIT_CAPACITY=EXPANDED claims=3 target=20Gi preservationSamples=2\n" {
		t.Fatalf("invalid receipt: %s", &out)
	}
}

func TestReadOnlyPreflightNeverConsumesOrPatches(t *testing.T) {
	f := setup(t)
	var out bytes.Buffer
	if code := run([]string{"--preflight"}, &out, f.execute, func(context.Context) bool { return true }); code != 0 || f.receipt || len(f.patched) > 0 || !strings.Contains(out.String(), "OPENBAO_AUDIT_CAPACITY=PREFLIGHT_OK") {
		t.Fatalf("read-only preflight missing or mutated state: %d receipt=%t patches=%v %s", code, f.receipt, f.patched, &out)
	}
}

func TestHoldReportsSafeFailureStage(t *testing.T) {
	for _, mode := range []string{"pricing", "source", "service health", "foreign workflow"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			if mode == "foreign workflow" {
				t.Setenv("GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/other.yaml@refs/heads/main")
			}
			if mode == "service health" {
				f.mode = "sealed"
			}
			wrapped := func(ctx context.Context, p string, a ...string) ([]byte, error) {
				if mode == "source" && p == "bash" {
					return []byte("PROD_RECOVERY_SOURCE=UNKNOWN"), nil
				}
				return f.execute(ctx, p, a...)
			}
			want := map[string]string{"pricing": "pricing", "source": "source", "service health": "service-health", "foreign workflow": "input"}[mode]
			var out bytes.Buffer
			if run([]string{"--preflight"}, &out, wrapped, func(context.Context) bool { return mode != "pricing" }) != 2 || f.receipt || len(f.patched) > 0 || !strings.Contains(out.String(), "stage="+want) || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("missing safe stage or state changed: %s", &out)
			}
		})
	}
}

func TestUnsafeEvidenceCannotExpand(t *testing.T) {
	for _, mode := range []string{"wrong binding", "foreign driver", "terminating claim", "larger capacity", "expansion disabled", "sealed", "missing resource"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.mode = mode
			c := f.claims["audit-openbao-0"]
			v := f.volumes["volume-0"]
			switch mode {
			case "wrong binding":
				v["spec"].(map[string]any)["claimRef"].(map[string]any)["uid"] = "replacement"
			case "foreign driver":
				v["spec"].(map[string]any)["csi"].(map[string]any)["driver"] = "other"
			case "terminating claim":
				c["metadata"].(map[string]any)["deletionTimestamp"] = "now"
			case "larger capacity":
				c["status"].(map[string]any)["capacity"].(map[string]any)["storage"] = "30Gi"
			case "missing resource":
				delete(f.claims, "audit-openbao-0")
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, f.execute, func(context.Context) bool { return true }); code != 2 || f.receipt || len(f.patched) > 0 || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("unsafe evidence accepted or leaked: %d %s", code, &out)
			}
		})
	}
}

func TestCostAndConfirmationGateBeforeCredentials(t *testing.T) {
	for _, mode := range []string{"unapproved", "retry", "over budget"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			if mode == "unapproved" {
				t.Setenv("AUDIT_CAPACITY_CONFIRM", "wrong")
			}
			if mode == "retry" {
				t.Setenv("GITHUB_RUN_ATTEMPT", "2")
			}
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, f.execute, func(context.Context) bool { return mode != "over budget" }) != 2 || f.receipt || len(f.patched) > 0 {
				t.Fatal("authority or budget gate bypassed")
			}
		})
	}
}

func TestAmbiguousOrConflictingWriteIsNotRetried(t *testing.T) {
	for _, mode := range []string{"conflict", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.mode = mode
			var out bytes.Buffer
			if run([]string{"--execute"}, &out, f.execute, func(context.Context) bool { return true }) != 2 || !f.receipt || len(f.patched) > 1 || !strings.Contains(out.String(), "HOLD_CAPACITY_MAY_HAVE_CHANGED") || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("ambiguous capacity write replayed or misreported: %s", &out)
			}
		})
	}
}
