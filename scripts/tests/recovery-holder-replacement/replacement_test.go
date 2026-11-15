package replacement_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type object = map[string]any

const (
	sha           = "6e2693c866bc045f03254f8a3e249796652180a0"
	podPath       = "/api/v1/namespaces/kubescape/pods/node-agent-tbvw8"
	daemonSetPath = "/apis/apps/v1/namespaces/kubescape/daemonsets/node-agent"
	volumePath    = "/api/v1/persistentvolumes/pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57"
	attachPath    = "/apis/storage.k8s.io/v1/volumeattachments/csi-b501cc5616bf0a961429f49e20f13940dc79989d4555eaeb836eef45dab94fa5"
	backendPath   = "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57"
	daemonSetUID  = "c90590f8-03c0-44d0-a06e-02dcf91439a9"
)

// Keep the real shell, JSON parser and kubectl; only the API and git are
// replaced. The fixture's DaemonSet replaces a deleted pod on its node, and
// the volume then detaches unless the test says the holder was another.
type apiFixture struct {
	mu             sync.Mutex
	objects        map[string]object
	detaches       bool
	readyReplace   bool
	readFailure    int
	servingChanged bool
	replaceDelete  bool
	deleted        []string
}

func (a *apiFixture) add(path, version, kind, name, namespace, uid string, body object) object {
	metadata := object{"name": name, "uid": uid, "resourceVersion": "1"}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	data := object{"apiVersion": version, "kind": kind, "metadata": metadata}
	for key, value := range body {
		data[key] = value
	}
	a.objects[path] = data
	return data
}

func nodeAgent(a *apiFixture, path, name, uid, node string, ready bool) {
	status := "False"
	if ready {
		status = "True"
	}
	pod := a.add(path, "v1", "Pod", name, "kubescape", uid, object{
		"spec":   object{"nodeName": node},
		"status": object{"phase": "Running", "conditions": []object{{"type": "Ready", "status": status}}},
	})
	metadata := pod["metadata"].(object)
	metadata["labels"] = object{"pod-template-generation": "40", "controller-revision-hash": "797d57f68c"}
	metadata["ownerReferences"] = []object{{"apiVersion": "apps/v1", "kind": "DaemonSet", "name": "node-agent", "uid": daemonSetUID, "controller": true}}
}

func newAPI() *apiFixture {
	a := &apiFixture{objects: map[string]object{}, detaches: true, readyReplace: true}
	for _, name := range []string{"private-mail-data", "email-data-encrypted"} {
		a.add("/api/v1/namespaces/private-mail/persistentvolumeclaims/"+name, "v1", "PersistentVolumeClaim", name, "private-mail", "uid-"+name, object{"spec": object{"volumeName": "pvc-serving-" + name}, "status": object{"phase": "Bound"}})
	}
	volume := a.add(volumePath, "v1", "PersistentVolume", "pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57", "", "01b86e2f-b35b-49bd-bcf9-ddc3c714cfaf", object{
		"spec": object{"storageClassName": "email-data-recovery-42818236850", "claimRef": object{"namespace": "email-data-recovery-42818236850", "name": "data", "uid": "fa4442ce-698b-46b2-a442-ba3ff2e48065"}},
	})
	volume["metadata"].(object)["deletionTimestamp"] = "2026-11-15T05:55:43Z"
	a.add(attachPath, "storage.k8s.io/v1", "VolumeAttachment", filepath.Base(attachPath), "", "c284cfa0-5f90-4130-93dd-f6f8d039e380", object{
		"spec":   object{"nodeName": "prod-worker-1", "attacher": "driver.longhorn.io", "source": object{"persistentVolumeName": "pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57"}},
		"status": object{"attached": true},
	})
	a.add(backendPath, "longhorn.io/v1beta2", "Volume", "pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57", "longhorn-system", "7fb51fcd-1ae0-49f1-9611-c06088ac506b", object{
		"status": object{"state": "attached", "kubernetesStatus": object{"namespace": "email-data-recovery-42818236850", "pvcName": "data", "pvName": "pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57", "lastPodRefAt": "2026-11-15T05:55:23Z", "lastPVCRefAt": "2026-11-15T05:55:43Z"}},
	})
	nodeAgent(a, podPath, "node-agent-tbvw8", "e8199e91-6ffe-4390-a269-12b7f8e13a20", "prod-worker-1", true)
	nodeAgent(a, "/api/v1/namespaces/kubescape/pods/node-agent-other", "node-agent-other", "uid-other-node", "prod-worker-2", true)
	a.add(daemonSetPath, "apps/v1", "DaemonSet", "node-agent", "kubescape", daemonSetUID, object{
		"spec":   object{"updateStrategy": object{"type": "RollingUpdate", "rollingUpdate": object{"maxUnavailable": 1, "maxSurge": 0}}},
		"status": object{"observedGeneration": 40, "desiredNumberScheduled": 6, "currentNumberScheduled": 6, "numberReady": 6, "updatedNumberScheduled": 6, "numberAvailable": 6, "numberMisscheduled": 0},
	})
	a.objects[daemonSetPath]["metadata"].(object)["generation"] = 40
	return a
}

func (a *apiFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(data)
	}
	status := func(code int, reason string) {
		write(code, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason, "code": code, "message": reason})
	}
	path := r.URL.Path
	if r.Method == http.MethodGet {
		var resources []object
		switch path {
		case "/api":
			write(200, object{"apiVersion": "v1", "kind": "APIVersions", "versions": []string{"v1"}})
			return
		case "/apis":
			groups := []object{}
			for group, version := range map[string]string{"storage.k8s.io": "v1", "longhorn.io": "v1beta2", "apps": "v1"} {
				v := object{"groupVersion": group + "/" + version, "version": version}
				groups = append(groups, object{"name": group, "versions": []object{v}, "preferredVersion": v})
			}
			write(200, object{"apiVersion": "v1", "kind": "APIGroupList", "groups": groups})
			return
		case "/api/v1":
			resources = []object{{"name": "namespaces", "singularName": "namespace", "kind": "Namespace", "namespaced": false}, {"name": "pods", "singularName": "pod", "kind": "Pod", "namespaced": true}, {"name": "persistentvolumes", "singularName": "persistentvolume", "kind": "PersistentVolume", "shortNames": []string{"pv"}, "namespaced": false}, {"name": "persistentvolumeclaims", "singularName": "persistentvolumeclaim", "kind": "PersistentVolumeClaim", "shortNames": []string{"pvc"}, "namespaced": true}}
		case "/apis/storage.k8s.io/v1":
			resources = []object{{"name": "volumeattachments", "singularName": "volumeattachment", "kind": "VolumeAttachment", "namespaced": false}}
		case "/apis/longhorn.io/v1beta2":
			resources = []object{{"name": "volumes", "singularName": "volume", "kind": "Volume", "namespaced": true}}
		case "/apis/apps/v1":
			resources = []object{{"name": "daemonsets", "singularName": "daemonset", "kind": "DaemonSet", "shortNames": []string{"ds"}, "namespaced": true}}
		}
		if resources != nil {
			for _, resource := range resources {
				resource["verbs"] = []string{"get", "list", "delete", "watch"}
			}
			write(200, object{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": strings.TrimPrefix(strings.TrimPrefix(path, "/apis/"), "/api/"), "resources": resources})
			return
		}
		if a.readFailure != 0 && (strings.Contains(path, "/pods") || strings.Contains(path, "/daemonsets")) {
			status(a.readFailure, http.StatusText(a.readFailure))
			return
		}
		if data, ok := a.objects[path]; ok {
			if a.servingChanged && len(a.deleted) > 0 && strings.Contains(path, "/private-mail/") {
				data["metadata"].(object)["uid"] = "changed-serving-uid"
			}
			write(200, data)
			return
		}
		items := []object{}
		keys := []string{}
		for key := range a.objects {
			if strings.HasPrefix(key, path+"/") && !strings.Contains(strings.TrimPrefix(key, path+"/"), "/") {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 || path == "/apis/storage.k8s.io/v1/volumeattachments" || strings.HasSuffix(path, "/pods") {
			sort.Strings(keys)
			for _, key := range keys {
				items = append(items, a.objects[key])
			}
			write(200, object{"apiVersion": "v1", "kind": "List", "items": items})
			return
		}
		status(404, "NotFound")
		return
	}
	if r.Method != http.MethodDelete {
		status(405, "MethodNotAllowed")
		return
	}
	a.deleted = append(a.deleted, path)
	data, ok := a.objects[path]
	if !ok {
		status(404, "NotFound")
		return
	}
	if a.replaceDelete {
		data["metadata"].(object)["uid"] = "replacement-uid"
	}
	var options struct {
		Preconditions struct{ UID string }
	}
	_ = json.NewDecoder(r.Body).Decode(&options)
	if options.Preconditions.UID == "" || options.Preconditions.UID != data["metadata"].(object)["uid"] {
		status(409, "Conflict")
		return
	}
	delete(a.objects, path)
	if path == podPath {
		nodeAgent(a, "/api/v1/namespaces/kubescape/pods/node-agent-new", "node-agent-new", "uid-new", "prod-worker-1", a.readyReplace)
		if a.detaches {
			delete(a.objects, attachPath)
			delete(a.objects, volumePath)
		}
	}
	write(200, object{"apiVersion": "v1", "kind": "Status", "status": "Success", "code": 200})
}

func runReplacement(t *testing.T, api *apiFixture, mainSHA string) (string, error) {
	t.Helper()
	for _, tool := range []string{"kubectl", "jq", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("native workflow dependency %s is required: %v", tool, err)
		}
	}
	raw, err := os.ReadFile("../../../.github/workflows/replace-recovery-volume-holder.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Name, Run string } }
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	script := ""
	for _, step := range workflow.Jobs["replace"].Steps {
		if step.Name == "Verify and replace the holder pod" {
			script = step.Run
		}
	}
	if !strings.Contains(script, "replacement_timeout=600\n") || !strings.Contains(script, "detach_timeout=600\n") {
		t.Fatal("the production replacement step or its waits are missing")
	}
	// Shorter waits only; every check and the order of operations are the step's own.
	script = strings.Replace(script, "replacement_timeout=600\n", "replacement_timeout=6\n", 1)
	script = strings.Replace(script, "detach_timeout=600\n", "detach_timeout=3\n", 1)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/usr/bin/env bash\n[[ \"$*\" == 'ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main' ]] || exit 9\nprintf '%s\\trefs/heads/main\\n' "+mainSHA+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	defer server.Close()
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\nusers:\n- name: fixture\n  user: {}\n", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KUBE_CONFIG=" + config, "GITHUB_SHA=" + sha}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("replacement stalled against the isolated API: %s", output)
	}
	return string(output), err
}

func refusedBeforeDeleting(t *testing.T, a *apiFixture, stage string) {
	t.Helper()
	output, err := runReplacement(t, a, sha)
	if err == nil || len(a.deleted) != 0 || !strings.Contains(output, "stage="+stage) {
		t.Fatalf("want refusal at %s before any delete: err=%v deleted=%v output=%s", stage, err, a.deleted, output)
	}
}

func TestReplacesOnlyThePinnedPodAndObservesOrdinaryDetach(t *testing.T) {
	a := newAPI()
	output, err := runReplacement(t, a, sha)
	if err != nil || !strings.Contains(output, "=DELETED pod=node-agent-tbvw8") || !strings.Contains(output, "=REPLACED replacement=node-agent-new/uid-new") || !strings.Contains(output, "=COMPLETE") {
		t.Fatalf("replacement did not complete: err=%v output=%s", err, output)
	}
	if len(a.deleted) != 1 || a.deleted[0] != podPath {
		t.Fatalf("only the pinned pod may be deleted, deleted %v", a.deleted)
	}
	if _, ok := a.objects["/api/v1/namespaces/kubescape/pods/node-agent-other"]; !ok {
		t.Fatal("another node's pod was touched")
	}
}

func TestDetachThatDoesNotFollowIsReportedNotRetried(t *testing.T) {
	a := newAPI()
	a.detaches = false
	output, err := runReplacement(t, a, sha)
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 3 || !strings.Contains(output, "=DETACH_NOT_OBSERVED") || strings.Contains(output, "=COMPLETE") || len(a.deleted) != 1 {
		t.Fatalf("a holder inference that the detach did not confirm must be reported once: err=%v deleted=%v output=%s", err, a.deleted, output)
	}
	if _, ok := a.objects[attachPath]; !ok {
		t.Fatal("the attachment must never be deleted")
	}
}

func TestReplacementThatNeverBecomesReadyFails(t *testing.T) {
	a := newAPI()
	a.readyReplace = false
	output, err := runReplacement(t, a, sha)
	if err == nil || !strings.Contains(output, "stage=replacement-timeout") || strings.Contains(output, "=REPLACED") || len(a.deleted) != 1 {
		t.Fatalf("an unready replacement was accepted: err=%v deleted=%v output=%s", err, a.deleted, output)
	}
}

func TestStaleMainRefusesBeforeAnyDeletion(t *testing.T) {
	a := newAPI()
	output, err := runReplacement(t, a, "0000000000000000000000000000000000000000")
	if err == nil || len(a.deleted) != 0 || !strings.Contains(output, "stage=current-main") {
		t.Fatalf("a superseded main was accepted: err=%v output=%s", err, output)
	}
}

func TestChangedPinsRefuseBeforeAnyDeletion(t *testing.T) {
	for _, tt := range []struct {
		name, stage string
		mutate      func(*apiFixture)
	}{
		{"pod replaced", "holder", func(a *apiFixture) { a.objects[podPath]["metadata"].(object)["uid"] = "uid-other" }},
		{"pod on another node", "holder", func(a *apiFixture) { a.objects[podPath]["spec"].(object)["nodeName"] = "prod-worker-2" }},
		{"pod already terminating", "holder", func(a *apiFixture) { a.objects[podPath]["metadata"].(object)["deletionTimestamp"] = "2026-11-15T07:00:00Z" }},
		{"pod from an older template", "holder", func(a *apiFixture) {
			a.objects[podPath]["metadata"].(object)["labels"].(object)["pod-template-generation"] = "39"
		}},
		{"template moved", "daemon-set", func(a *apiFixture) { a.objects[daemonSetPath]["metadata"].(object)["generation"] = 41 }},
		{"daemon set degraded", "daemon-set", func(a *apiFixture) { a.objects[daemonSetPath]["status"].(object)["numberReady"] = 5 }},
		{"daemon set replaced", "daemon-set", func(a *apiFixture) { a.objects[daemonSetPath]["metadata"].(object)["uid"] = "uid-other" }},
		{"consumer namespace back", "consumer-present", func(a *apiFixture) {
			a.add("/api/v1/namespaces/email-data-recovery-42818236850", "v1", "Namespace", "email-data-recovery-42818236850", "", "uid-ns", nil)
		}},
		{"volume not deleting", "volume", func(a *apiFixture) { delete(a.objects[volumePath]["metadata"].(object), "deletionTimestamp") }},
		{"volume of another claim", "volume", func(a *apiFixture) {
			a.objects[volumePath]["spec"].(object)["claimRef"].(object)["uid"] = "uid-other"
		}},
		{"attachment on another node", "attachment", func(a *apiFixture) { a.objects[attachPath]["spec"].(object)["nodeName"] = "prod-worker-2" }},
		{"second attachment", "attachment", func(a *apiFixture) {
			a.add("/apis/storage.k8s.io/v1/volumeattachments/csi-second", "storage.k8s.io/v1", "VolumeAttachment", "csi-second", "", "uid-second", object{"spec": object{"source": object{"persistentVolumeName": "pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57"}}})
		}},
		{"backend still used by a pod", "backend", func(a *apiFixture) {
			a.objects[backendPath]["status"].(object)["kubernetesStatus"].(object)["lastPodRefAt"] = ""
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newAPI()
			tt.mutate(a)
			refusedBeforeDeleting(t, a, tt.stage)
		})
	}
}

func TestFailedReadsRefuseBeforeAnyDeletion(t *testing.T) {
	for _, code := range []int{403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			a := newAPI()
			a.readFailure = code
			output, err := runReplacement(t, a, sha)
			if err == nil || len(a.deleted) != 0 || strings.Contains(output, "=DELETED") {
				t.Fatalf("a failed read became clearance: err=%v output=%s", err, output)
			}
		})
	}
}

func TestReplacedPodDuringDeleteIsNotDeleted(t *testing.T) {
	a := newAPI()
	a.replaceDelete = true
	output, err := runReplacement(t, a, sha)
	if err == nil || !strings.Contains(output, "stage=delete") || strings.Contains(output, "=DELETED") {
		t.Fatalf("a delete without the pinned UID went through: err=%v output=%s", err, output)
	}
	if _, ok := a.objects[podPath]; !ok {
		t.Fatal("the replaced pod was deleted")
	}
}

func TestChangedServingIdentityPreventsCompletion(t *testing.T) {
	a := newAPI()
	a.servingChanged = true
	output, err := runReplacement(t, a, sha)
	if err == nil || strings.Contains(output, "=COMPLETE") {
		t.Fatalf("a changed serving claim was not detected: err=%v output=%s", err, output)
	}
}
