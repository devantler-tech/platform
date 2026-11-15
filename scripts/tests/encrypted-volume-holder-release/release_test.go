package release_test

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
	sha          = "6e2693c866bc045f03254f8a3e249796652180a0"
	volume       = "pvc-6a149990-5a81-496d-bf0a-54f347e9cd9d"
	holderPath   = "/api/v1/namespaces/kubescape/pods/node-agent-holder"
	otherPath    = "/api/v1/namespaces/kubescape/pods/node-agent-other"
	daemonSet    = "/apis/apps/v1/namespaces/kubescape/daemonsets/node-agent"
	attachPath   = "/apis/storage.k8s.io/v1/volumeattachments/csi-old"
	pluginPath   = "/api/v1/namespaces/longhorn-system/pods/longhorn-csi-plugin-old"
	consumerPath = "/api/v1/namespaces/private-mail/pods/private-mail-0"
	daemonSetUID = "uid-daemon-set"
	stuckClose   = "time=\"2026-11-15T08:42:28Z\" level=error msg=\"NodeUnstageVolume: err: rpc error: code = Internal desc = failed after 10 attempts to refresh stale namespace directory: failed to execute: /usr/bin/nsenter [nsenter --mount=/host/proc/4731/ns/mnt --ipc=/host/proc/4731/ns/ipc cryptsetup luksClose " + volume + "], output , stderr Device " + volume + " is still in use.: exit status 5\"\n"
)

// Keep the real shell, JSON parser and kubectl; only the API and git are
// replaced. The fixture models the mail store moving from worker-2 to
// worker-1 while worker-2's node agent holds the device.
type apiFixture struct {
	mu           sync.Mutex
	objects      map[string]object
	pluginLog    string
	detaches     bool
	readyReplace bool
	readFailure  int
	replaceUID   bool
	deleted      []string
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

func (a *apiFixture) nodeAgent(path, name, uid, node string, ready bool) {
	status := "False"
	if ready {
		status = "True"
	}
	pod := a.add(path, "v1", "Pod", name, "kubescape", uid, object{
		"spec":   object{"nodeName": node},
		"status": object{"phase": "Running", "conditions": []object{{"type": "Ready", "status": status}}},
	})
	metadata := pod["metadata"].(object)
	// No legacy pod-template-generation label: the current template is
	// identified only by the DaemonSet's highest ControllerRevision.
	metadata["labels"] = object{"controller-revision-hash": "797d57f68c"}
	metadata["ownerReferences"] = []object{{"apiVersion": "apps/v1", "kind": "DaemonSet", "name": "node-agent", "uid": daemonSetUID, "controller": true}}
}

func newAPI() *apiFixture {
	a := &apiFixture{objects: map[string]object{}, pluginLog: stuckClose, detaches: true, readyReplace: true}
	a.add("/api/v1/persistentvolumes/"+volume, "v1", "PersistentVolume", volume, "", "uid-volume", object{
		"spec": object{"csi": object{"driver": "driver.longhorn.io"}, "claimRef": object{"namespace": "private-mail", "name": "email-data-encrypted"}},
	})
	a.add("/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/"+volume, "longhorn.io/v1beta2", "Volume", volume, "longhorn-system", "uid-backend", object{"spec": object{"encrypted": true}})
	a.add(attachPath, "storage.k8s.io/v1", "VolumeAttachment", "csi-old", "", "uid-attachment-old", object{
		"spec":   object{"nodeName": "prod-worker-2", "attacher": "driver.longhorn.io", "source": object{"persistentVolumeName": volume}},
		"status": object{"attached": true},
	})
	a.add(consumerPath, "v1", "Pod", "private-mail-0", "private-mail", "uid-mail", object{
		"spec":   object{"nodeName": "prod-worker-1", "volumes": []object{{"name": "data", "persistentVolumeClaim": object{"claimName": "email-data-encrypted"}}}},
		"status": object{"phase": "Pending"},
	})
	plugin := object{"containers": []object{{"name": "longhorn-csi-plugin", "image": "longhornio/longhorn-manager"}}}
	a.add(pluginPath, "v1", "Pod", "longhorn-csi-plugin-old", "longhorn-system", "uid-plugin-old", object{"spec": object{"nodeName": "prod-worker-2", "containers": plugin["containers"]}})
	a.add("/api/v1/namespaces/longhorn-system/pods/longhorn-csi-plugin-new", "v1", "Pod", "longhorn-csi-plugin-new", "longhorn-system", "uid-plugin-new", object{"spec": object{"nodeName": "prod-worker-1", "containers": plugin["containers"]}})
	a.nodeAgent(holderPath, "node-agent-holder", "uid-holder", "prod-worker-2", true)
	a.nodeAgent(otherPath, "node-agent-other", "uid-other", "prod-worker-1", true)
	a.add(daemonSet, "apps/v1", "DaemonSet", "node-agent", "kubescape", daemonSetUID, object{
		"spec":   object{"updateStrategy": object{"type": "RollingUpdate"}},
		"status": object{"observedGeneration": 40, "desiredNumberScheduled": 6, "currentNumberScheduled": 6, "numberReady": 6, "updatedNumberScheduled": 6, "numberAvailable": 6, "numberMisscheduled": 0},
	})
	a.objects[daemonSet]["metadata"].(object)["generation"] = 40
	for revision, hash := range map[int]string{42: "58649fd754", 43: "797d57f68c"} {
		data := a.add("/apis/apps/v1/namespaces/kubescape/controllerrevisions/node-agent-"+hash, "apps/v1", "ControllerRevision", "node-agent-"+hash, "kubescape", "uid-revision-"+hash, object{"revision": revision})
		data["metadata"].(object)["labels"] = object{"controller-revision-hash": hash}
		data["metadata"].(object)["ownerReferences"] = []object{{"apiVersion": "apps/v1", "kind": "DaemonSet", "name": "node-agent", "uid": daemonSetUID, "controller": true}}
	}
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
			resources = []object{{"name": "pods", "singularName": "pod", "kind": "Pod", "namespaced": true}, {"name": "pods/log", "singularName": "", "kind": "Pod", "namespaced": true}, {"name": "persistentvolumes", "singularName": "persistentvolume", "kind": "PersistentVolume", "shortNames": []string{"pv"}, "namespaced": false}}
		case "/apis/storage.k8s.io/v1":
			resources = []object{{"name": "volumeattachments", "singularName": "volumeattachment", "kind": "VolumeAttachment", "namespaced": false}}
		case "/apis/longhorn.io/v1beta2":
			resources = []object{{"name": "volumes", "singularName": "volume", "kind": "Volume", "namespaced": true}}
		case "/apis/apps/v1":
			resources = []object{{"name": "daemonsets", "singularName": "daemonset", "kind": "DaemonSet", "shortNames": []string{"ds"}, "namespaced": true}, {"name": "controllerrevisions", "singularName": "controllerrevision", "kind": "ControllerRevision", "namespaced": true}}
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
		if path == pluginPath+"/log" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(a.pluginLog))
			return
		}
		if data, ok := a.objects[path]; ok {
			write(200, data)
			return
		}
		items := []object{}
		keys := []string{}
		for key := range a.objects {
			rest, found := strings.CutPrefix(key, path+"/")
			if path == "/api/v1/pods" {
				rest, found = "", strings.Contains(key, "/pods/")
			}
			if found && !strings.Contains(rest, "/") {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 || strings.HasSuffix(path, "/volumeattachments") || strings.HasSuffix(path, "/pods") || strings.HasSuffix(path, "/controllerrevisions") {
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
	if a.replaceUID {
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
	if path == holderPath {
		a.nodeAgent("/api/v1/namespaces/kubescape/pods/node-agent-new", "node-agent-new", "uid-new", "prod-worker-2", a.readyReplace)
		if a.detaches {
			delete(a.objects, attachPath)
			a.add("/apis/storage.k8s.io/v1/volumeattachments/csi-new", "storage.k8s.io/v1", "VolumeAttachment", "csi-new", "", "uid-attachment-new", object{
				"spec":   object{"nodeName": "prod-worker-1", "attacher": "driver.longhorn.io", "source": object{"persistentVolumeName": volume}},
				"status": object{"attached": true},
			})
		}
	}
	write(200, object{"apiVersion": "v1", "kind": "Status", "status": "Success", "code": 200})
}

func runRelease(t *testing.T, api *apiFixture, input, mainSHA string) (string, error) {
	t.Helper()
	for _, tool := range []string{"kubectl", "jq", "bash", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("native workflow dependency %s is required: %v", tool, err)
		}
	}
	raw, err := os.ReadFile("../../../.github/workflows/release-encrypted-volume-holder.yaml")
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
	for _, step := range workflow.Jobs["release"].Steps {
		if step.Name == "Verify and release the holder" {
			script = step.Run
		}
	}
	if !strings.Contains(script, "replacement_timeout=600\n") || !strings.Contains(script, "detach_timeout=600\n") {
		t.Fatal("the production release step or its waits are missing")
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
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KUBE_CONFIG=" + config, "GITHUB_SHA=" + sha, "VOLUME=" + input}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("release stalled against the isolated API: %s", output)
	}
	return string(output), err
}

func refusedBeforeDeleting(t *testing.T, a *apiFixture, stage string) {
	t.Helper()
	output, err := runRelease(t, a, volume, sha)
	if err == nil || len(a.deleted) != 0 || !strings.Contains(output, "stage="+stage) {
		t.Fatalf("want refusal at %s before any delete: err=%v deleted=%v output=%s", stage, err, a.deleted, output)
	}
}

func TestReleasesTheOneHolderAndObservesTheOrdinaryMove(t *testing.T) {
	a := newAPI()
	output, err := runRelease(t, a, volume, sha)
	if err != nil || !strings.Contains(output, "=OBSERVED volume="+volume+" node=prod-worker-2") || !strings.Contains(output, "=DELETED pod=node-agent-holder") || !strings.Contains(output, "=REPLACED replacement=node-agent-new/uid-new") || !strings.Contains(output, "=COMPLETE") {
		t.Fatalf("release did not complete: err=%v output=%s", err, output)
	}
	if len(a.deleted) != 1 || a.deleted[0] != holderPath {
		t.Fatalf("only the holder may be deleted, deleted %v", a.deleted)
	}
	if _, ok := a.objects[otherPath]; !ok {
		t.Fatal("another node's agent was touched")
	}
}

func TestDetachThatDoesNotFollowIsReportedNotRetried(t *testing.T) {
	a := newAPI()
	a.detaches = false
	output, err := runRelease(t, a, volume, sha)
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 3 || !strings.Contains(output, "=DETACH_NOT_OBSERVED") || strings.Contains(output, "=COMPLETE") || len(a.deleted) != 1 {
		t.Fatalf("an unconfirmed holder inference must be reported once: err=%v deleted=%v output=%s", err, a.deleted, output)
	}
	if _, ok := a.objects[attachPath]; !ok {
		t.Fatal("the attachment must never be deleted")
	}
}

func TestReplacementThatNeverBecomesReadyFails(t *testing.T) {
	a := newAPI()
	a.readyReplace = false
	output, err := runRelease(t, a, volume, sha)
	if err == nil || !strings.Contains(output, "stage=replacement-timeout") || strings.Contains(output, "=REPLACED") {
		t.Fatalf("an unready replacement was accepted: err=%v output=%s", err, output)
	}
}

func TestInvalidInputsRefuseBeforeAnyRead(t *testing.T) {
	for _, input := range []string{"", "pvc-6a149990", "private-mail-data", volume + " --all", "../" + volume} {
		a := newAPI()
		output, err := runRelease(t, a, input, sha)
		if err == nil || len(a.deleted) != 0 || !strings.Contains(output, "stage=volume-name") {
			t.Fatalf("input %q was accepted: err=%v output=%s", input, err, output)
		}
	}
}

func TestStaleMainRefusesBeforeAnyDeletion(t *testing.T) {
	a := newAPI()
	output, err := runRelease(t, a, volume, "0000000000000000000000000000000000000000")
	if err == nil || len(a.deleted) != 0 || !strings.Contains(output, "stage=current-main") {
		t.Fatalf("a superseded main was accepted: err=%v output=%s", err, output)
	}
}

func TestPreconditionsRefuseBeforeAnyDeletion(t *testing.T) {
	for _, tt := range []struct {
		name, stage string
		mutate      func(*apiFixture)
	}{
		{"not encrypted", "backend", func(a *apiFixture) {
			a.objects["/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/"+volume]["spec"].(object)["encrypted"] = false
		}},
		{"another driver", "volume", func(a *apiFixture) {
			a.objects["/api/v1/persistentvolumes/"+volume]["spec"].(object)["csi"].(object)["driver"] = "other"
		}},
		{"not attached", "attachment", func(a *apiFixture) { delete(a.objects, attachPath) }},
		{"two attachments", "attachment", func(a *apiFixture) {
			a.add("/apis/storage.k8s.io/v1/volumeattachments/csi-second", "storage.k8s.io/v1", "VolumeAttachment", "csi-second", "", "uid-second", object{"spec": object{"nodeName": "prod-worker-1", "attacher": "driver.longhorn.io", "source": object{"persistentVolumeName": volume}}, "status": object{"attached": true}})
		}},
		{"consumer still on the node", "in-use-on-node", func(a *apiFixture) {
			a.objects[consumerPath]["spec"].(object)["nodeName"] = "prod-worker-2"
		}},
		{"no stuck close logged", "no-stuck-close", func(a *apiFixture) { a.pluginLog = "level=info msg=\"NodeUnstageVolume succeeded\"\n" }},
		{"stuck close of another volume", "no-stuck-close", func(a *apiFixture) {
			a.pluginLog = strings.ReplaceAll(stuckClose, volume, "pvc-00000000-0000-0000-0000-000000000000")
		}},
		{"daemon set degraded", "daemon-set", func(a *apiFixture) { a.objects[daemonSet]["status"].(object)["numberReady"] = 5 }},
		{"daemon set rolling", "daemon-set", func(a *apiFixture) { a.objects[daemonSet]["status"].(object)["observedGeneration"] = 39 }},
		{"holder from an older template", "holder", func(a *apiFixture) {
			a.objects[holderPath]["metadata"].(object)["labels"].(object)["controller-revision-hash"] = "58649fd754"
		}},
		{"no revisions", "revision", func(a *apiFixture) {
			for key := range a.objects {
				if strings.Contains(key, "/controllerrevisions/") {
					delete(a.objects, key)
				}
			}
		}},
		{"current revision of another owner", "holder", func(a *apiFixture) {
			a.objects["/apis/apps/v1/namespaces/kubescape/controllerrevisions/node-agent-797d57f68c"]["metadata"].(object)["ownerReferences"].([]object)[0]["uid"] = "uid-other-owner"
		}},
		{"holder terminating", "holder", func(a *apiFixture) { a.objects[holderPath]["metadata"].(object)["deletionTimestamp"] = "2026-11-15T09:00:00Z" }},
		{"no holder on the node", "holder", func(a *apiFixture) { delete(a.objects, holderPath) }},
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
			output, err := runRelease(t, a, volume, sha)
			if err == nil || len(a.deleted) != 0 || strings.Contains(output, "=DELETED") {
				t.Fatalf("a failed read became clearance: err=%v output=%s", err, output)
			}
		})
	}
}

func TestReplacedHolderDuringDeleteIsNotDeleted(t *testing.T) {
	a := newAPI()
	a.replaceUID = true
	output, err := runRelease(t, a, volume, sha)
	if err == nil || !strings.Contains(output, "stage=delete") || strings.Contains(output, "=DELETED") {
		t.Fatalf("a delete without the observed UID went through: err=%v output=%s", err, output)
	}
	if _, ok := a.objects[holderPath]; !ok {
		t.Fatal("the replaced pod was deleted")
	}
}
