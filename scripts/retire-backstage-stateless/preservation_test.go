package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRetainedObjectsCannotDependOnAnyRetiringIdentity(t *testing.T) {
	for _, uid := range []string{"release-uid", "chart-uid", "deployment-uid", "service-uid", "configmap-uid", "rs-uid", "pod-uid"} {
		for _, kind := range []string{"Secret", "PersistentVolumeClaim", "PersistentVolume"} {
			t.Run(kind+"_"+uid, func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				r := item(f.inventory, 0)
				switch kind {
				case "PersistentVolumeClaim":
					r = item(f.live, 3)
				case "PersistentVolume":
					r = item(f.volumes, 0)
				}
				object(r["metadata"])["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "retiring", "uid": uid}}
				var out bytes.Buffer
				if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.configured || f.deleted || !strings.Contains(out.String(), "HOLD_PRECONDITION") {
					t.Fatalf("retained dependency reached mutation: code=%d %s", code, &out)
				}
			})
		}
	}
}

func TestMalformedOwnershipCannotAuthorizeRetirement(t *testing.T) {
	for _, kind := range []string{"HelmChart", "Secret", "PersistentVolume", "HelmHistory"} {
		for _, owners := range []any{"invalid", map[string]any{"uid": "unbound"}, []any{map[string]any{"kind": "Cluster", "name": "backstage-db"}}} {
			t.Run(fmt.Sprintf("%s_%T", kind, owners), func(t *testing.T) {
				retirementRunner(t)
				f := retirementFixture(t)
				r := f.chart
				switch kind {
				case "Secret":
					r = item(f.inventory, 0)
				case "PersistentVolume":
					r = item(f.volumes, 0)
				case "HelmHistory":
					r = item(f.inventory, 1)
				}
				object(r["metadata"])["ownerReferences"] = owners
				var out bytes.Buffer
				if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.deleted {
					t.Fatalf("malformed ownership accepted: code=%d %s", code, &out)
				}
			})
		}
	}
}

func TestPruneAnnotationCannotSubstitutePhysicalRetention(t *testing.T) {
	for _, annotation := range []any{nil, "disabled", "enabled", "false", true} {
		t.Run(fmt.Sprint(annotation), func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			object(item(f.live, 3)["metadata"])["annotations"] = map[string]any{"kustomize.toolkit.fluxcd.io/prune": annotation}
			object(item(f.volumes, 0)["spec"])["persistentVolumeReclaimPolicy"] = "Delete"
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.configured || f.deleted || !strings.Contains(out.String(), "HOLD_PRECONDITION") {
				t.Fatalf("claim annotation substituted for physical storage retention: code=%d %s", code, &out)
			}
		})
	}
}

func TestHistoricalHelmPayloadCannotChangeAcrossAnySample(t *testing.T) {
	for _, afterDelete := range []bool{false, true} {
		t.Run(fmt.Sprint(afterDelete), func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			history := historicalSecret(t, f, 3, "superseded")
			f.inventory["items"] = append(f.inventory["items"].([]any), history)
			reads := 0
			execute := func(c context.Context, p string, a ...string) ([]byte, error) {
				if p == "kubectl" && strings.Contains(strings.Join(a, " "), "get service,configmap,secret") {
					reads++
					if !afterDelete && reads == 2 || afterDelete && f.deleted {
						history["data"] = map[string]any{"release": "PRIVATE_CHANGED_PAYLOAD"}
					}
				}
				return f.execute(c, p, a...)
			}
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, execute); code != 2 || !afterDelete && (f.receiptCreated || f.deleted) || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("historical payload change accepted or leaked: code=%d %s", code, &out)
			}
		})
	}
}

func historicalSecret(t *testing.T, f *portalFixture, version int, status string) map[string]any {
	t.Helper()
	r, err := decode(encode(t, f.release))
	if err != nil {
		t.Fatal(err)
	}
	r["version"] = version
	object(r["info"])["status"] = status
	s := nativeReleaseSecret(t, r)
	object(s["metadata"])["name"] = fmt.Sprintf("sh.helm.release.v1.backstage.v%d", version)
	object(s["metadata"])["uid"] = fmt.Sprintf("history-uid-%d", version)
	object(s["metadata"])["labels"] = map[string]any{"owner": "helm", "name": "backstage", "version": fmt.Sprint(version), "status": status}
	return s
}

func TestEveryHelmRevisionNeedsCompleteConsistentStoredContent(t *testing.T) {
	for _, mode := range []string{"corrupt", "empty", "foreign version", "foreign name", "foreign status", "older deployed"} {
		t.Run(mode, func(t *testing.T) {
			retirementRunner(t)
			f := retirementFixture(t)
			s := historicalSecret(t, f, 3, "superseded")
			switch mode {
			case "corrupt":
				s["data"] = map[string]any{"release": "PRIVATE_CORRUPT_PAYLOAD"}
			case "empty":
				delete(s, "data")
			case "foreign version":
				object(s["metadata"])["labels"] = map[string]any{"owner": "helm", "name": "backstage", "version": "2", "status": "superseded"}
				object(s["metadata"])["name"] = "sh.helm.release.v1.backstage.v2"
			case "foreign name":
				r, _ := decode(encode(t, f.release))
				r["name"], r["version"] = "other", 3
				object(r["info"])["status"] = "superseded"
				s["data"] = nativeReleaseSecret(t, r)["data"]
			case "foreign status":
				object(s["metadata"])["labels"].(map[string]any)["status"] = "failed"
			case "older deployed":
				s = historicalSecret(t, f, 3, "deployed")
			}
			f.inventory["items"] = append(f.inventory["items"].([]any), s)
			var out bytes.Buffer
			if code := run([]string{"--execute"}, &out, f.execute); code != 2 || f.receiptCreated || f.deleted || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("unproven stored history reached mutation or leaked: code=%d %s", code, &out)
			}
		})
	}
}

func TestOnlyNativeUninstallInformationMayChange(t *testing.T) {
	for _, key := range []string{"status", "deleted", "description", "resources", "notes", "first_deployed", "last_deployed", "unknown"} {
		t.Run(key, func(t *testing.T) {
			original, _ := fixture()
			changed, err := decode(encode(t, original))
			if err != nil {
				t.Fatal(err)
			}
			object(changed["info"])[key] = "changed"
			allowed := key == "status" || key == "deleted" || key == "description" || key == "resources"
			if sameStoredContent(original, changed) != allowed {
				t.Fatalf("unexpected stored information exception: field=%s allowed=%t", key, allowed)
			}
		})
	}
}
