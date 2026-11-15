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
	// talos holds the node's answers for the stub Talos client, keyed by
	// "<before|after>/<command><path with / as _>". Nil makes every Talos read fail.
	talos    map[string]string
	talosDir string
	// evidenceTimeout, when set, replaces the step's evidence bound in seconds.
	evidenceTimeout int
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
	a.add("/api/v1/nodes/prod-worker-2", "v1", "Node", "prod-worker-2", "", "uid-node", object{
		"status": object{"addresses": []object{{"type": "Hostname", "address": "prod-worker-2"}, {"type": "InternalIP", "address": "10.0.0.2"}}},
	})
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
			resources = []object{{"name": "pods", "singularName": "pod", "kind": "Pod", "namespaced": true}, {"name": "pods/log", "singularName": "", "kind": "Pod", "namespaced": true}, {"name": "persistentvolumes", "singularName": "persistentvolume", "kind": "PersistentVolume", "shortNames": []string{"pv"}, "namespaced": false}, {"name": "nodes", "singularName": "node", "kind": "Node", "shortNames": []string{"no"}, "namespaced": false}}
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
		if a.talosDir != "" {
			// The node now answers with the replacement node-agent.
			_ = os.WriteFile(filepath.Join(a.talosDir, "replaced"), nil, 0o600)
		}
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
	for _, tool := range []string{"kubectl", "jq", "bash", "grep", "timeout"} {
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
	if !strings.Contains(script, "replacement_timeout=600\n") || !strings.Contains(script, "detach_timeout=600\n") || !strings.Contains(script, "evidence_timeout=120\n") {
		t.Fatal("the production release step or its waits are missing")
	}
	// Shorter waits only; every check and the order of operations are the step's own.
	script = strings.Replace(script, "replacement_timeout=600\n", "replacement_timeout=6\n", 1)
	script = strings.Replace(script, "detach_timeout=600\n", "detach_timeout=3\n", 1)
	if api.evidenceTimeout != 0 {
		script = strings.Replace(script, "evidence_timeout=120\n", fmt.Sprintf("evidence_timeout=%d\n", api.evidenceTimeout), 1)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/usr/bin/env bash\n[[ \"$*\" == 'ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main' ]] || exit 9\nprintf '%s\\trefs/heads/main\\n' "+mainSHA+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTalos(t, api, bin)
	server := httptest.NewServer(api)
	defer server.Close()
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\nusers:\n- name: fixture\n  user: {}\n", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KUBE_CONFIG=" + config, "TALOS_CONFIG=fixture-talosconfig", "GITHUB_SHA=" + sha, "VOLUME=" + input}
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

// A CSI plugin that has retried for long logs many matching failures; reading
// them must not end in SIGPIPE and refuse the recovery.
func TestLongStuckCloseLogIsRecognised(t *testing.T) {
	a := newAPI()
	a.pluginLog = strings.Repeat(stuckClose, 20000)
	output, err := runRelease(t, a, volume, sha)
	if err != nil || !strings.Contains(output, "=COMPLETE") {
		t.Fatalf("a long log with the stuck close was refused: err=%v output=%s", err, output)
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
		{"holder terminating", "holder", func(a *apiFixture) {
			a.objects[holderPath]["metadata"].(object)["deletionTimestamp"] = "2026-11-15T09:00:00Z"
		}},
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

// writeTalos installs a stub Talos client that answers from api.talos, so the
// evidence reads run their real shell, awk and xargs against fixed node state.
// It answers only for the fixture's talosconfig and node address.
func writeTalos(t *testing.T, api *apiFixture, bin string) {
	t.Helper()
	dir := filepath.Join(bin, "talos-data")
	api.talosDir = dir
	for _, phase := range []string{"before", "after"} {
		if err := os.MkdirAll(filepath.Join(dir, phase), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if api.talos == nil {
		if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range api.talos {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stub := `#!/usr/bin/env bash
d="$(dirname "$0")/talos-data"
[[ -f "$d/fail" ]] && exit 1
[[ -f "$d/slow" ]] && sleep 5
[[ "$1" == --talosconfig && "$(cat "$2")" == fixture-talosconfig && "$3" == -n && "$4" == 10.0.0.2 ]] || exit 9
shift 4
phase=before
[[ -f "$d/replaced" ]] && phase=after
case "$1" in
  processes)
    # A later listing in the same phase answers from processes-again, if given.
    f="$d/$phase/processes"
    [[ -f "$d/$phase/processes-again" && -f "$d/$phase/.listed" ]] && f="$d/$phase/processes-again"
    : >"$d/$phase/.listed" ;;
  list) [[ "$2" == -l ]] || exit 9; f="$d/$phase/list$(printf '%s' "$3" | tr / _)" ;;
  read) f="$d/$phase/read$(printf '%s' "$2" | tr / _)" ;;
  *) exit 9 ;;
esac
[[ -f "$f" ]] || exit 1
cat "$f"
`
	if err := os.WriteFile(filepath.Join(bin, "talosctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
}

// processTable renders `talosctl processes`, whose LABEL column can hold spaces.
func processTable(rows ...[2]string) string {
	line := func(pid, label, command string) string {
		return fmt.Sprintf("%-12s%-9s%-8s%-10s%-11s%-10s%-9s%-38s%s\n", "10.0.0.2", pid, "S", "8", "1.00", "1 GB", "1 MB", label, command)
	}
	out := fmt.Sprintf("%-12s%-9s%-8s%-10s%-11s%-10s%-9s%-38s%s\n", "NODE", "PID", "STATE", "THREADS", "CPU-TIME", "VIRTMEM", "RESMEM", "LABEL", "COMMAND")
	for _, row := range rows {
		label := "unconfined"
		if strings.HasPrefix(row[1], "postgres") {
			label = "cri-containerd.apparmor.d (enforce)"
		}
		out += line(row[0], label, row[1])
	}
	return out
}

// links renders `talosctl list -l` entries that are symbolic links.
func links(entries ...string) string {
	out := "NODE        MODE         UID   GID   SIZE(B)   LASTMOD           LABEL   NAME\n"
	for _, entry := range entries {
		out += "10.0.0.2    Lrwx------   0     0     64        Nov 15 22:13:57           " + entry + "\n"
	}
	return out
}

const privateMount = "/var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/abc/globalmount/mailbox-private"

// nodeAtHold is a node whose node-agent (pid 100) holds one mount and one
// network namespace no process is in, and whose own mount namespace and the
// host's still contain the volume's device 253:3. After the replacement, agent
// pid 101 holds only live namespaces and the device is gone.
func nodeAtHold() map[string]string {
	return map[string]string{
		"before/processes":               processTable([2]string{"1", "/sbin/init"}, [2]string{"100", "/usr/bin/node-agent"}, [2]string{"200", "postgres -D /var/lib/postgresql/data"}, [2]string{"300", "/velero node-agent server"}),
		"before/list_proc_100_fd":        links("3 -> socket:[999]", "54 -> mnt:[11]", "55 -> net:[21]", "56 -> mnt:[13]", "57 -> net:[23]", "58 -> mnt:[12]"),
		"before/list_proc_1_ns":          links("mnt -> mnt:[10]", "net -> net:[20]"),
		"before/list_proc_100_ns":        links("mnt -> mnt:[12]", "net -> net:[20]"),
		"before/list_proc_200_ns":        links("mnt -> mnt:[11]", "net -> net:[21]"),
		"before/list_proc_300_ns":        links("mnt -> mnt:[14]", "net -> net:[20]"),
		"before/list_dev_mapper":         links("pvc-0b0d7f1e-0000-4000-8000-000000000001 -> ../dm-1", volume+" -> ../dm-3"),
		"before/read_sys_block_dm-3_dev": "253:3\n",
		"before/read_proc_1_mountinfo":   "36 35 253:3 / " + privateMount + " rw,relatime shared:7 - ext4 /dev/mapper/" + volume + " rw\n24 1 8:1 / / rw shared:1 - ext4 /dev/sda1 rw\n",
		"before/read_proc_100_mountinfo": "236 235 253:3 / /host" + privateMount + " rw,relatime master:7 - ext4 /dev/mapper/" + volume + " rw\n",
		"before/read_proc_200_mountinfo": "40 39 8:1 / / rw - ext4 /dev/sda1 rw\n",
		"before/read_proc_300_mountinfo": "50 49 8:1 / / rw - ext4 /dev/sda1 rw\n",
		"after/processes":                processTable([2]string{"1", "/sbin/init"}, [2]string{"101", "/usr/bin/node-agent"}, [2]string{"200", "postgres -D /var/lib/postgresql/data"}),
		"after/list_proc_101_fd":         links("54 -> mnt:[11]", "55 -> net:[21]", "56 -> mnt:[15]"),
		"after/list_proc_1_ns":           links("mnt -> mnt:[10]", "net -> net:[20]"),
		"after/list_proc_101_ns":         links("mnt -> mnt:[15]", "net -> net:[20]"),
		"after/list_proc_200_ns":         links("mnt -> mnt:[11]", "net -> net:[21]"),
		"after/list_dev_mapper":          links("pvc-0b0d7f1e-0000-4000-8000-000000000001 -> ../dm-1"),
	}
}

func TestRecordsHolderEvidenceBeforeAndAfterTheRelease(t *testing.T) {
	a := newAPI()
	a.talos = nodeAtHold()
	output, err := runRelease(t, a, volume, sha)
	if err != nil || !strings.Contains(output, "=COMPLETE") {
		t.Fatalf("release did not complete: err=%v output=%s", err, output)
	}
	for _, want := range []string{
		"ENCRYPTED_VOLUME_HOLDER_EVIDENCE=NAMESPACE phase=before process=node-agent mounts=1 shared=0 slave=1 private=0",
		"ENCRYPTED_VOLUME_HOLDER_EVIDENCE=NAMESPACE phase=before process=init mounts=1 shared=1 slave=0 private=0",
		"ENCRYPTED_VOLUME_HOLDER_EVIDENCE=RECORDED phase=before agent_pid=100 held_mnt=3 dead_mnt=1 held_net=2 dead_net=1 device=253:3 namespaces_with_device=2 agent_namespace_has_device=yes",
		"ENCRYPTED_VOLUME_HOLDER_EVIDENCE=RECORDED phase=after agent_pid=101 held_mnt=2 dead_mnt=0 held_net=1 dead_net=0 device=absent namespaces_with_device=0 agent_namespace_has_device=no",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
	// Evidence comes before the only mutation, and again after the detach wait.
	if before, deleted := strings.Index(output, "phase=before agent_pid"), strings.Index(output, "=DELETED"); before < 0 || deleted < before {
		t.Fatalf("before-evidence must precede the delete:\n%s", output)
	}
	if after, complete := strings.Index(output, "phase=after agent_pid"), strings.Index(output, "=COMPLETE"); after < 0 || complete < after {
		t.Fatalf("after-evidence must precede the completion:\n%s", output)
	}
	// Nothing from a path, a mount or a command line reaches the log.
	for _, leak := range []string{"/var/lib", "mailbox-private", "globalmount", "postgresql", "/dev/mapper"} {
		if strings.Contains(output, leak) {
			t.Fatalf("evidence leaked %q:\n%s", leak, output)
		}
	}
	if len(a.deleted) != 1 || a.deleted[0] != holderPath {
		t.Fatalf("only the holder may be deleted, deleted %v", a.deleted)
	}
}

func TestUnreadableEvidenceNeverChangesTheRelease(t *testing.T) {
	a := newAPI() // every Talos read fails
	output, err := runRelease(t, a, volume, sha)
	if err != nil || !strings.Contains(output, "=COMPLETE") || len(a.deleted) != 1 {
		t.Fatalf("release must complete without evidence: err=%v deleted=%v output=%s", err, a.deleted, output)
	}
	for _, want := range []string{"EVIDENCE=UNAVAILABLE phase=before stage=processes", "EVIDENCE=UNAVAILABLE phase=after stage=processes"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
	if strings.Contains(output, "EVIDENCE=RECORDED") {
		t.Fatalf("unreadable evidence was reported as recorded:\n%s", output)
	}
}

func TestEvidenceRefusesToGuessWhatItCannotRead(t *testing.T) {
	cases := map[string]struct {
		edit  func(map[string]string)
		stage string
	}{
		"two node-agent processes": {func(n map[string]string) {
			n["before/processes"] = processTable([2]string{"1", "/sbin/init"}, [2]string{"100", "/usr/bin/node-agent"}, [2]string{"101", "/usr/bin/node-agent"})
		}, "agent"},
		"no node address":        {nil, "node-address"},
		"unreadable descriptors": {func(n map[string]string) { delete(n, "before/list_proc_100_fd") }, "agent-descriptors"},
		"volume listed in an unexpected form": {func(n map[string]string) {
			n["before/list_dev_mapper"] = links(volume + " -> /dev/dm-3")
		}, "device"},
		"unreadable device number": {func(n map[string]string) { n["before/read_sys_block_dm-3_dev"] = "not-a-device\n" }, "device"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAPI()
			a.talos = nodeAtHold()
			if c.edit != nil {
				c.edit(a.talos)
			} else {
				delete(a.objects, "/api/v1/nodes/prod-worker-2")
			}
			output, err := runRelease(t, a, volume, sha)
			if err != nil || !strings.Contains(output, "=COMPLETE") || len(a.deleted) != 1 {
				t.Fatalf("release must complete: err=%v deleted=%v output=%s", err, a.deleted, output)
			}
			if !strings.Contains(output, "EVIDENCE=UNAVAILABLE phase=before stage="+c.stage) || strings.Contains(output, "RECORDED phase=before") {
				t.Fatalf("want before-evidence UNAVAILABLE at %s:\n%s", c.stage, output)
			}
		})
	}
}

// A failed read counts as absent only once a second listing shows its process
// has exited; a failed read of a process that still runs is no evidence.
func TestFailedProcessReadsAreNotMistakenForAbsence(t *testing.T) {
	stillRunning := processTable([2]string{"1", "/sbin/init"}, [2]string{"100", "/usr/bin/node-agent"}, [2]string{"200", "postgres -D /var/lib/postgresql/data"}, [2]string{"300", "/velero node-agent server"})
	exited := processTable([2]string{"1", "/sbin/init"}, [2]string{"100", "/usr/bin/node-agent"}, [2]string{"200", "postgres -D /var/lib/postgresql/data"})
	recorded := "EVIDENCE=RECORDED phase=before agent_pid=100 held_mnt=3 dead_mnt=1 held_net=2 dead_net=1 device=253:3 namespaces_with_device=2 agent_namespace_has_device=yes"
	cases := map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"namespaces of a running process": {func(n map[string]string) {
			delete(n, "before/list_proc_200_ns")
			n["before/processes-again"] = stillRunning
		}, "EVIDENCE=UNAVAILABLE phase=before stage=namespaces"},
		"mounts of a running process": {func(n map[string]string) {
			delete(n, "before/read_proc_300_mountinfo")
			n["before/processes-again"] = stillRunning
		}, "EVIDENCE=UNAVAILABLE phase=before stage=mounts"},
		"namespaces of a process that exited": {func(n map[string]string) {
			delete(n, "before/list_proc_300_ns")
			n["before/processes-again"] = exited
		}, recorded},
		"mounts of a process that exited": {func(n map[string]string) {
			delete(n, "before/read_proc_300_mountinfo")
			n["before/processes-again"] = exited
		}, recorded},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAPI()
			a.talos = nodeAtHold()
			c.edit(a.talos)
			output, err := runRelease(t, a, volume, sha)
			if err != nil || !strings.Contains(output, "=COMPLETE") || len(a.deleted) != 1 {
				t.Fatalf("release must complete: err=%v deleted=%v output=%s", err, a.deleted, output)
			}
			if !strings.Contains(output, c.want) {
				t.Fatalf("missing %q in:\n%s", c.want, output)
			}
			if strings.Contains(c.want, "UNAVAILABLE") && strings.Contains(output, "phase=before process=") {
				t.Fatalf("partial evidence was printed:\n%s", output)
			}
		})
	}
}

// The bound ends every read it started: a slow node reports UNAVAILABLE at
// the deadline and the release goes on at once.
func TestEvidenceBoundEndsSlowReads(t *testing.T) {
	a := newAPI()
	a.talos = nodeAtHold()
	a.talos["slow"] = ""
	a.evidenceTimeout = 1
	started := time.Now()
	output, err := runRelease(t, a, volume, sha)
	elapsed := time.Since(started)
	if err != nil || !strings.Contains(output, "=COMPLETE") || len(a.deleted) != 1 {
		t.Fatalf("release must complete: err=%v deleted=%v output=%s", err, a.deleted, output)
	}
	for _, want := range []string{"EVIDENCE=UNAVAILABLE phase=before stage=deadline", "EVIDENCE=UNAVAILABLE phase=after stage=deadline"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
	// Each slow read takes five seconds; two one-second bounds must not wait for them.
	if elapsed > 8*time.Second {
		t.Fatalf("the evidence bound did not end its reads: the release took %s", elapsed)
	}
}
