package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

var observation = time.Date(2026, 10, 4, 21, 0, 0, 0, time.UTC)

func resource(kind, name, uid string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": name, "uid": uid}}
}

func baseline() map[string]map[string]any {
	sc := resource("StorageClass", "durable", "class-id")
	sc["apiVersion"] = "storage.k8s.io/v1"
	sc["reclaimPolicy"] = "Retain"
	pv := resource("PersistentVolume", "data", "volume-id")
	pv["spec"] = map[string]any{"persistentVolumeReclaimPolicy": "Retain", "storageClassName": "durable", "claimRef": map[string]any{"namespace": "app", "name": "claim", "uid": "claim-id"}}
	pv["status"] = map[string]any{"phase": "Bound"}
	pvc := resource("PersistentVolumeClaim", "claim", "claim-id")
	pvc["metadata"].(map[string]any)["namespace"] = "app"
	pvc["spec"] = map[string]any{"volumeName": "data", "storageClassName": "durable"}
	pvc["status"] = map[string]any{"phase": "Bound"}
	list := func(kind, api string, items ...any) map[string]any {
		if items == nil {
			items = []any{}
		}
		return map[string]any{"kind": kind + "List", "apiVersion": api, "metadata": map[string]any{"resourceVersion": "123", "continue": ""}, "items": items}
	}
	return map[string]map[string]any{
		"classes.json": list("StorageClass", "storage.k8s.io/v1", sc),
		"volumes.json": list("PersistentVolume", "v1", pv),
		"claims.json":  list("PersistentVolumeClaim", "v1", pvc),
		"uploads.json": list("DataUpload", "velero.io/v2alpha1"),
		"capture.json": {"startedAt": observation.Add(-time.Minute).Format(time.RFC3339), "completedAt": observation.Format(time.RFC3339)},
	}
}

func first(data map[string]map[string]any, file string) map[string]any {
	return data[file]["items"].([]any)[0].(map[string]any)
}

func snapshot(t *testing.T, data map[string]map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	for name, document := range data {
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAuditsRealPoliciesAndReleaseAge(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		change                    func(map[string]map[string]any)
		status                    string
		classes, volumes, overdue int
	}{
		{"retained", func(_ map[string]map[string]any) {}, "CLEAN", 0, 0, 0},
		{"delete class", func(d map[string]map[string]any) { first(d, "classes.json")["reclaimPolicy"] = "Delete" }, "WARNING", 1, 0, 0},
		{"delete volume", func(d map[string]map[string]any) {
			first(d, "volumes.json")["spec"].(map[string]any)["persistentVolumeReclaimPolicy"] = "Delete"
		}, "WARNING", 0, 1, 0},
		{"released overdue", func(d map[string]map[string]any) {
			first(d, "volumes.json")["status"] = map[string]any{"phase": "Released", "lastPhaseTransitionTime": observation.Add(-2 * time.Hour).Format(time.RFC3339)}
			d["claims.json"]["items"] = []any{}
		}, "WARNING", 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := baseline()
			tc.change(d)
			r, err := audit(snapshot(t, d), observation, time.Hour)
			if err != nil || r.Status != tc.status || r.Classes != 1 || r.Volumes != 1 || r.DeleteClasses != tc.classes || r.DeleteVolumes != tc.volumes || r.OverdueReleasedVolumes != tc.overdue {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
}

func TestNativeTypedListMayOmitItemTypeHeaders(t *testing.T) {
	d := baseline()
	for _, file := range []string{"classes.json", "volumes.json", "claims.json"} {
		delete(first(d, file), "apiVersion")
		delete(first(d, file), "kind")
	}
	r, err := audit(snapshot(t, d), observation, time.Hour)
	if err != nil || r.Status != "CLEAN" || r.Volumes != 1 {
		t.Fatalf("native typed list: %+v %v", r, err)
	}
	d = baseline()
	first(d, "volumes.json")["kind"] = "Secret"
	if _, err := audit(snapshot(t, d), observation, time.Hour); err == nil {
		t.Fatal("explicit wrong item type passed")
	}
}

func temporary(d map[string]map[string]any) {
	pv := first(d, "volumes.json")
	pvc := first(d, "claims.json")
	pv["spec"].(map[string]any)["persistentVolumeReclaimPolicy"] = "Delete"
	pv["spec"].(map[string]any)["claimRef"].(map[string]any)["namespace"] = "velero"
	pvc["metadata"].(map[string]any)["namespace"] = "velero"
	pvc["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "velero.io/v2alpha1", "kind": "DataUpload", "name": "claim", "uid": "upload-id", "controller": true}}
	pvc["spec"].(map[string]any)["dataSource"] = map[string]any{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": "snapshot"}
	upload := resource("DataUpload", "claim", "upload-id")
	upload["apiVersion"] = "velero.io/v2alpha1"
	upload["metadata"].(map[string]any)["namespace"] = "velero"
	d["uploads.json"]["items"] = []any{upload}
}

func TestBackupExemptionNeedsUIDJoinedControllerOwnership(t *testing.T) {
	d := baseline()
	temporary(d)
	r, err := audit(snapshot(t, d), observation, time.Hour)
	if err != nil || r.Status != "CLEAN" || r.TemporaryVolumes != 1 || r.DeleteVolumes != 0 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	d = baseline()
	temporary(d)
	delete(first(d, "claims.json")["metadata"].(map[string]any), "ownerReferences")
	r, err = audit(snapshot(t, d), observation, time.Hour)
	if err != nil || r.Status != "WARNING" || r.DeleteVolumes != 1 || r.TemporaryVolumes != 0 {
		t.Fatalf("namespace alone exempted volume: %+v %v", r, err)
	}
	d = baseline()
	temporary(d)
	first(d, "uploads.json")["metadata"].(map[string]any)["uid"] = "recreated-upload"
	if _, err := audit(snapshot(t, d), observation, time.Hour); err == nil {
		t.Fatal("stale owner UID reported clean")
	}
}

func TestIncompleteOrUnjoinableObservationIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]map[string]any)
	}{
		{"partial page", func(d map[string]map[string]any) { d["volumes.json"]["metadata"].(map[string]any)["continue"] = "next" }},
		{"missing items", func(d map[string]map[string]any) { delete(d["volumes.json"], "items") }},
		{"missing list version", func(d map[string]map[string]any) {
			delete(d["claims.json"]["metadata"].(map[string]any), "resourceVersion")
		}},
		{"API error", func(d map[string]map[string]any) {
			d["uploads.json"] = map[string]any{"kind": "Status", "message": "forbidden"}
		}},
		{"empty fleet", func(d map[string]map[string]any) { d["volumes.json"]["items"] = []any{} }},
		{"missing claim", func(d map[string]map[string]any) { d["claims.json"]["items"] = []any{} }},
		{"claim UID mismatch", func(d map[string]map[string]any) {
			first(d, "claims.json")["metadata"].(map[string]any)["uid"] = "recreated"
		}},
		{"wrong binding", func(d map[string]map[string]any) {
			first(d, "claims.json")["spec"].(map[string]any)["volumeName"] = "other"
		}},
		{"duplicate identity", func(d map[string]map[string]any) {
			d["volumes.json"]["items"] = append(d["volumes.json"]["items"].([]any), first(d, "volumes.json"))
		}},
		{"missing phase time", func(d map[string]map[string]any) {
			first(d, "volumes.json")["status"] = map[string]any{"phase": "Released"}
			d["claims.json"]["items"] = []any{}
		}},
		{"stale capture", func(d map[string]map[string]any) {
			d["capture.json"]["startedAt"] = observation.Add(-time.Hour).Format(time.RFC3339)
		}},
		{"future capture", func(d map[string]map[string]any) {
			d["capture.json"]["completedAt"] = observation.Add(time.Hour).Format(time.RFC3339)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := baseline()
			tc.change(d)
			if _, err := audit(snapshot(t, d), observation, time.Hour); err == nil {
				t.Fatal("incomplete observation reported clean")
			}
		})
	}
	if _, err := audit(t.TempDir(), observation, time.Hour); err == nil {
		t.Fatal("unreadable input reported clean")
	}
}

func TestFailedCaptureCannotLeaveCompletionReceipt(t *testing.T) {
	commands := t.TempDir()
	stub := "#!/bin/sh\ncase \"$*\" in *datauploads*) exit 1;; esac\nprintf '%s\\n' '{}'\n"
	if err := os.WriteFile(filepath.Join(commands, "kubectl"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "capture")
	command := exec.Command("bash", "capture.sh", "reader", dir)
	command.Env = append(os.Environ(), "PATH="+commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("failed API read accepted: %s", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "capture.json")); !os.IsNotExist(err) {
		t.Fatalf("completion receipt exists after failure: %v", err)
	}
}
