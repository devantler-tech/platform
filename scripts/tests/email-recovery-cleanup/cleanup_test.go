package cleanup_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type object = map[string]any

// The reviewed identities the workflow pins (#6041). The fixture serves the
// same names and UIDs, so a changed pin fails these tests.
type identity struct {
	copy, namespaceUID, claimUID, volume, volumeUID, backendUID, classUID string
}

var first = identity{"41561215415", "", "97658b9b-8a22-4f02-8275-697bec198e46", "pvc-ba88e988-bd96-4828-bf08-05054047af95", "d04d4704-bb6c-436b-85a1-62ce3ec0899a", "8d9c0f45-2337-4284-b67b-2f0a715151c6", "80c442fe-9dcb-4bbb-99c0-919f39821c45"}
var second = identity{"42818236850", "d3c5fc1d-43ef-45e6-a581-58de881875a2", "fa4442ce-698b-46b2-a442-ba3ff2e48065", "pvc-898b45a4-11cb-4075-9c25-6a3a6bbd9e57", "01b86e2f-b35b-49bd-bcf9-ddc3c714cfaf", "7fb51fcd-1ae0-49f1-9611-c06088ac506b", "4886f450-1d12-4bb0-8ee4-f3313edaddd0"}

// Keep the real shell, JSON parser and kubectl. Only the remote API is replaced;
// in particular kubectl must transmit DeleteOptions rather than a name-only delete.
type apiFixture struct {
	mu                 sync.Mutex
	objects            map[string]object
	readFailure        int
	postDeleteFailure  bool
	servingChanged     bool
	replaceDelete      int
	replacementDeleted bool
	deletes            int
	deleted            []string
}

func paths(id identity) (namespace, claim, volume, backend, class, attachment string) {
	name := "email-data-recovery-" + id.copy
	return "/api/v1/namespaces/" + name,
		"/api/v1/namespaces/" + name + "/persistentvolumeclaims/data",
		"/api/v1/persistentvolumes/" + id.volume,
		"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/" + id.volume,
		"/apis/storage.k8s.io/v1/storageclasses/" + name,
		"/apis/storage.k8s.io/v1/volumeattachments/csi-" + id.copy
}

func (a *apiFixture) add(path, version, kind, name, namespace, uid string, body object) {
	metadata := object{"name": name, "uid": uid, "resourceVersion": "1"}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	data := object{"apiVersion": version, "kind": kind, "metadata": metadata}
	for key, value := range body {
		data[key] = value
	}
	a.objects[path] = data
}

// addCopy serves one copy in the state its removal has reached: "full" while
// its reader still runs, "attached" after its namespace went but its volume
// stayed attached (run 43674222442), and "detached" once that volume is gone.
func (a *apiFixture) addCopy(id identity, state string) {
	name := "email-data-recovery-" + id.copy
	namespace, claim, volume, backend, class, attachment := paths(id)
	backendState := "detached"
	if state != "detached" {
		backendState = "attached"
	}
	a.add(backend, "longhorn.io/v1beta2", "Volume", id.volume, "longhorn-system", id.backendUID, object{
		"spec":   object{"encrypted": true},
		"status": object{"state": backendState, "kubernetesStatus": object{"namespace": name, "pvcName": "data", "pvName": id.volume}},
	})
	a.add(class, "storage.k8s.io/v1", "StorageClass", name, "", id.classUID, object{"provisioner": "driver.longhorn.io"})
	if state == "detached" {
		return
	}
	a.add(volume, "v1", "PersistentVolume", id.volume, "", id.volumeUID, object{
		"spec":   object{"storageClassName": name, "claimRef": object{"namespace": name, "name": "data", "uid": id.claimUID}},
		"status": object{"phase": "Bound"},
	})
	a.add(attachment, "storage.k8s.io/v1", "VolumeAttachment", "csi-"+id.copy, "", "uid-attachment-"+id.copy, object{
		"spec":   object{"attacher": "driver.longhorn.io", "source": object{"persistentVolumeName": id.volume}},
		"status": object{"attached": true},
	})
	if state == "attached" {
		a.objects[volume]["metadata"].(object)["deletionTimestamp"] = "2026-11-15T04:27:01Z"
		return
	}
	a.add(namespace, "v1", "Namespace", name, "", id.namespaceUID, nil)
	a.objects[namespace]["metadata"].(object)["labels"] = object{"app.kubernetes.io/part-of": "email-data-recovery", "email.devantler.tech/data-recovery": id.copy}
	a.add(claim, "v1", "PersistentVolumeClaim", "data", name, id.claimUID, object{"spec": object{"volumeName": id.volume}, "status": object{"phase": "Bound"}})
}

// newAPI serves production as it is after run 43674222442 and the node agents'
// propagation fix: the first copy detached, the second complete.
func newAPI() *apiFixture {
	a := &apiFixture{objects: map[string]object{}}
	for _, name := range []string{"private-mail-data", "email-data-encrypted"} {
		a.add("/api/v1/namespaces/private-mail/persistentvolumeclaims/"+name, "v1", "PersistentVolumeClaim", name, "private-mail", "uid-"+name, object{"spec": object{"volumeName": "pvc-serving-" + name}, "status": object{"phase": "Bound"}})
	}
	a.addCopy(first, "detached")
	a.addCopy(second, "full")
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
			for group, version := range map[string]string{"storage.k8s.io": "v1", "longhorn.io": "v1beta2"} {
				v := object{"groupVersion": group + "/" + version, "version": version}
				groups = append(groups, object{"name": group, "versions": []object{v}, "preferredVersion": v})
			}
			write(200, object{"apiVersion": "v1", "kind": "APIGroupList", "groups": groups})
			return
		case "/api/v1":
			resources = []object{{"name": "namespaces", "singularName": "namespace", "kind": "Namespace", "namespaced": false}, {"name": "persistentvolumes", "singularName": "persistentvolume", "kind": "PersistentVolume", "shortNames": []string{"pv"}, "namespaced": false}, {"name": "persistentvolumeclaims", "singularName": "persistentvolumeclaim", "kind": "PersistentVolumeClaim", "shortNames": []string{"pvc"}, "namespaced": true}}
		case "/apis/storage.k8s.io/v1":
			resources = []object{{"name": "storageclasses", "singularName": "storageclass", "kind": "StorageClass", "namespaced": false}, {"name": "volumeattachments", "singularName": "volumeattachment", "kind": "VolumeAttachment", "namespaced": false}}
		case "/apis/longhorn.io/v1beta2":
			resources = []object{{"name": "volumes", "singularName": "volume", "kind": "Volume", "namespaced": true}}
		}
		if resources != nil {
			for _, resource := range resources {
				resource["verbs"] = []string{"get", "list", "delete", "watch"}
			}
			write(200, object{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": strings.TrimPrefix(strings.TrimPrefix(path, "/apis/"), "/api/"), "resources": resources})
			return
		}
		if a.readFailure != 0 && (strings.HasPrefix(path, "/api/v1/namespaces/email-data-recovery-") || strings.Contains(path, "/storageclasses/email-data-recovery-")) {
			status(a.readFailure, http.StatusText(a.readFailure))
			return
		}
		if a.postDeleteFailure && a.deletes > 0 && strings.Contains(path, "recovery-") {
			status(500, "InternalError")
			return
		}
		if data, ok := a.objects[path]; ok {
			if a.servingChanged && a.deletes > 0 && strings.Contains(path, "/private-mail/") {
				data["metadata"].(object)["uid"] = "changed-serving-uid"
			}
			write(200, data)
			return
		}
		if path == "/api/v1/persistentvolumes" || path == "/apis/storage.k8s.io/v1/volumeattachments" || strings.HasSuffix(path, "/persistentvolumeclaims") {
			items := []object{}
			keys := []string{}
			for key := range a.objects {
				if strings.HasPrefix(key, path+"/") && !strings.Contains(strings.TrimPrefix(key, path+"/"), "/") {
					keys = append(keys, key)
				}
			}
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
	a.deletes++
	a.deleted = append(a.deleted, path)
	data, ok := a.objects[path]
	if !ok {
		status(404, "NotFound")
		return
	}
	if a.replaceDelete == a.deletes {
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
	if data["metadata"].(object)["uid"] == "replacement-uid" {
		a.replacementDeleted = true
	}
	delete(a.objects, path)
	if data["kind"] == "Namespace" {
		for key := range a.objects {
			if strings.HasPrefix(key, path+"/") {
				delete(a.objects, key)
			}
		}
		// The reader pod is gone, so the node unstages the volume and the
		// attach-detach controller removes its attachment: an ordinary detach.
		for _, id := range []identity{first, second} {
			if namespace, _, _, backend, _, attachment := paths(id); namespace == path {
				delete(a.objects, attachment)
				if volume, ok := a.objects[backend]; ok {
					volume["status"].(object)["state"] = "detached"
				}
			}
		}
	}
	write(200, object{"apiVersion": "v1", "kind": "Status", "status": "Success", "code": 200})
}

func runCleanup(t *testing.T, api *apiFixture) (string, error) {
	t.Helper()
	for _, tool := range []string{"kubectl", "jq", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("native workflow dependency %s is required: %v", tool, err)
		}
	}
	raw, err := os.ReadFile("../../../.github/workflows/remove-email-recovery-copies.yaml")
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
	for _, step := range workflow.Jobs["remove"].Steps {
		if step.Name == "Verify and remove both copies" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("the production removal step is missing")
	}
	server := httptest.NewServer(api)
	defer server.Close()
	// No ambient kubeconfig, credentials, discovery cache or production endpoint.
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\nusers:\n- name: fixture\n  user: {}\n", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KUBE_CONFIG=" + config}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("cleanup stalled against the isolated API: %s", output)
	}
	return string(output), err
}

func refusedBeforeDeleting(t *testing.T, a *apiFixture, stage string) {
	t.Helper()
	output, err := runCleanup(t, a)
	if err == nil || a.deletes != 0 || strings.Contains(output, "=REMOVED") || !strings.Contains(output, "stage="+stage) {
		t.Fatalf("want refusal at %s before any delete: err=%v deletes=%d output=%s", stage, err, a.deletes, output)
	}
}

func TestFailedReadsNeverReportReleasedStorage(t *testing.T) {
	for _, code := range []int{403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			a := newAPI()
			a.readFailure = code
			output, err := runCleanup(t, a)
			if err == nil || strings.Contains(output, "=COMPLETE") || strings.Contains(output, "=REMOVED") || a.deletes != 0 {
				t.Fatalf("failed API read became cleanup clearance: err=%v deletes=%d output=%s", err, a.deletes, output)
			}
		})
	}
}

func TestReplacedResourceIsNotDeleted(t *testing.T) {
	for index := 1; index <= 6; index++ {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			a := newAPI()
			a.replaceDelete = index
			output, err := runCleanup(t, a)
			if err == nil || a.replacementDeleted || strings.Contains(output, "=COMPLETE") {
				t.Fatalf("replacement was not preserved: err=%v replacementDeleted=%v output=%s", err, a.replacementDeleted, output)
			}
		})
	}
}

func TestCleanupRequiresSuccessfulAbsenceReads(t *testing.T) {
	a := newAPI()
	a.postDeleteFailure = true
	output, err := runCleanup(t, a)
	if err == nil || strings.Contains(output, "=REMOVED") || strings.Contains(output, "=COMPLETE") {
		t.Fatalf("uncertain deletion became completion: err=%v output=%s", err, output)
	}
}

func TestOutOfOrderAbsenceRefusesBeforeAnyDeletion(t *testing.T) {
	a := newAPI()
	namespace, _, _, _, class, _ := paths(second)
	delete(a.objects, namespace)
	delete(a.objects, class)
	refusedBeforeDeleting(t, a, "order")
}

func TestRecreatedNamespaceIsNotDeleted(t *testing.T) {
	a := newAPI()
	namespace, _, _, _, _, _ := paths(first)
	a.add(namespace, "v1", "Namespace", "email-data-recovery-"+first.copy, "", "uid-recreated", nil)
	refusedBeforeDeleting(t, a, "namespace-replaced")
}

func TestChangedPinsRefuseBeforeAnyDeletion(t *testing.T) {
	for _, tt := range []struct{ name, stage string }{{"volume", "volume-replaced"}, {"backend", "backend-replaced"}, {"class", "class-replaced"}} {
		t.Run(tt.name, func(t *testing.T) {
			a := newAPI()
			_, _, volume, backend, class, _ := paths(second)
			path := map[string]string{"volume": volume, "backend": backend, "class": class}[tt.name]
			a.objects[path]["metadata"].(object)["uid"] = "uid-other"
			refusedBeforeDeleting(t, a, tt.stage)
		})
	}
}

func TestAttachedVolumeRefusesBeforeAnyDeletion(t *testing.T) {
	a := newAPI()
	a.addCopy(first, "attached")
	refusedBeforeDeleting(t, a, "detach-pending")
}

func TestBackendInUseRefusesBeforeAnyDeletion(t *testing.T) {
	a := newAPI()
	_, _, _, backend, _, _ := paths(first)
	a.objects[backend]["status"].(object)["state"] = "attached"
	refusedBeforeDeleting(t, a, "backend-in-use")
}

func TestForeignBackendBindingRefusesBeforeAnyDeletion(t *testing.T) {
	a := newAPI()
	_, _, _, backend, _, _ := paths(first)
	a.objects[backend]["status"].(object)["kubernetesStatus"].(object)["namespace"] = "private-mail"
	refusedBeforeDeleting(t, a, "backend-binding")
}

func TestChangedServingIdentityPreventsCompletion(t *testing.T) {
	a := newAPI()
	a.servingChanged = true
	output, err := runCleanup(t, a)
	if err == nil || strings.Contains(output, "=COMPLETE") {
		t.Fatalf("serving replacement was not detected: err=%v output=%s", err, output)
	}
}

func TestResumedCopiesAreRemovedAndServingClaimsRetained(t *testing.T) {
	a := newAPI()
	output, err := runCleanup(t, a)
	if err != nil || !strings.Contains(output, "=COMPLETE serving=UNCHANGED") || strings.Count(output, "=REMOVED copy=") != 2 || a.deletes != 6 {
		t.Fatalf("resumed copies did not complete: err=%v deletes=%d output=%s", err, a.deletes, output)
	}
	for _, path := range a.deleted {
		if strings.Contains(path, "volumeattachments") || strings.Contains(path, "/private-mail/") {
			t.Fatalf("cleanup deleted %s; detach must stay ordinary and serving claims untouched", path)
		}
	}
	for path := range a.objects {
		if !strings.HasPrefix(path, "/api/v1/namespaces/private-mail/persistentvolumeclaims/") {
			t.Fatalf("recovery resource retained: %s", path)
		}
	}
	if len(a.objects) != 2 {
		t.Fatal("serving claims were altered")
	}
}
