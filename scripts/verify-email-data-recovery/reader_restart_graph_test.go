package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/emailreaderinputs"
)

type readerRestartAPI struct {
	inner              *cleanupAPI
	backend            object
	engine             object
	replicas           []object
	longhornAttachment object
	csiAttachment      object
	extraCSI           []object
	continuations      map[string]any
	mutations          int
}

// call models complete native list reads and rejects any Kubernetes mutation.
func (a *readerRestartAPI) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if len(input) != 0 || len(args) < 2 || args[0] != "get" {
		a.mutations++
		return nil, fmt.Errorf("unexpected native mutation")
	}
	listMetadata := object{}
	if token, present := a.continuations[args[1]]; present {
		listMetadata["continue"] = token
	}
	switch args[1] {
	case "volumes.longhorn.io":
		return json.Marshal(a.backend)
	case "engines.longhorn.io":
		return json.Marshal(object{"apiVersion": "longhorn.io/v1beta2", "kind": "EngineList", "metadata": listMetadata, "items": []object{a.engine}})
	case "replicas.longhorn.io":
		return json.Marshal(object{"apiVersion": "longhorn.io/v1beta2", "kind": "ReplicaList", "metadata": listMetadata, "items": a.replicas})
	case "volumeattachments.longhorn.io":
		return json.Marshal(a.longhornAttachment)
	case "volumeattachments.storage.k8s.io":
		return json.Marshal(object{"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachmentList", "metadata": listMetadata, "items": append([]object{a.csiAttachment}, a.extraCSI...)})
	default:
		return a.inner.call(ctx, nil, args...)
	}
}

// readerRestartNativeFixture binds the restored PV to a complete independent V1 attachment graph.
func readerRestartNativeFixture(t *testing.T) (*readerRestartAPI, map[string]string) {
	t.Helper()
	volume, env := readerVolumeNativeFixture(t)
	delete(env, "EMAIL_RETAINED_VOLUME_CONFIRM")
	env["GITHUB_WORKFLOW_REF"] = readerRestartDispatch()["GITHUB_WORKFLOW_REF"]
	env["EMAIL_READER_WITNESS_CONFIRM"] = "prepare-retained-email-reader-witness"
	env["EMAIL_RETAINED_BACKEND_UID"] = "aaaaaaaa-5555-6666-7777-888888888888"
	name := volume.name
	pod := volume.inner.objects[cleanupKey("Pod", retainedRecovery.namespace, "reader")]
	child(pod, "spec")["nodeName"] = "fixture-worker"
	child(volume.inner.objects[cleanupKey("PersistentVolume", "", name)], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	backendUID := env["EMAIL_RETAINED_BACKEND_UID"]
	meta := func(name, uid string) object {
		return object{"name": name, "namespace": "longhorn-system", "uid": uid, "resourceVersion": "700", "ownerReferences": []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": volume.name, "uid": backendUID}}}
	}
	a := &readerRestartAPI{inner: volume.inner}
	child(a.inner.objects[cleanupKey("Pod", "private-mail", "private-mail-0")], "metadata")["uid"] = "eeeeeeee-5555-6666-7777-888888888888"
	child(a.inner.objects[cleanupKey("PersistentVolume", "", "pvc-serving-volume")], "metadata")["uid"] = "eeeeeeee-6666-6666-7777-888888888888"
	a.backend = object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "metadata": object{"name": name, "namespace": "longhorn-system", "uid": backendUID, "resourceVersion": "600"}, "spec": object{"encrypted": true, "dataEngine": "v1", "frontend": "blockdev", "numberOfReplicas": float64(3), "size": "2147483648", "nodeID": "fixture-worker"}, "status": object{"state": "attached", "robustness": "healthy", "currentNodeID": "fixture-worker"}}
	modes, addresses := object{}, object{}
	var replicaUIDs []string
	for index := 1; index <= 3; index++ {
		uid := fmt.Sprintf("1000000%d-5555-6666-7777-888888888888", index)
		replicaUIDs = append(replicaUIDs, uid)
		replicaName := fmt.Sprintf("%s-r-%08d", name, index)
		modes[replicaName], addresses[replicaName] = "RW", fmt.Sprintf("tcp://fixture-%d:9502", index)
		a.replicas = append(a.replicas, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Replica", "metadata": meta(replicaName, uid), "spec": object{"volumeName": name, "active": true, "nodeID": fmt.Sprintf("fixture-node-%d", index), "diskID": fmt.Sprintf("fixture-disk-%d", index), "dataDirectoryName": "fixture-preserved", "dataEngine": "v1"}, "status": object{"currentState": "running"}})
	}
	raw, err := json.Marshal(replicaUIDs)
	if err != nil {
		t.Fatal(err)
	}
	env["EMAIL_RETAINED_REPLICA_UIDS"] = string(raw)
	a.engine = object{"apiVersion": "longhorn.io/v1beta2", "kind": "Engine", "metadata": meta(name+"-e-0", "bbbbbbbb-5555-6666-7777-888888888888"), "spec": object{"volumeName": name, "active": true, "nodeID": "fixture-worker", "frontend": "blockdev", "dataEngine": "v1", "replicaAddressMap": addresses}, "status": object{"currentState": "running", "endpoint": "/dev/longhorn/" + name, "replicaModeMap": modes}}
	attachment := "csi-" + strings.Repeat("a", 64)
	a.longhornAttachment = object{"apiVersion": "longhorn.io/v1beta2", "kind": "VolumeAttachment", "metadata": meta(name, "cccccccc-5555-6666-7777-888888888888"), "spec": object{"volume": name, "attachmentTickets": object{attachment: object{"id": attachment, "generation": float64(0), "type": "csi-attacher", "nodeID": "fixture-worker", "parameters": object{"disableFrontend": "false"}}}}, "status": object{"attachmentTicketStatuses": object{attachment: object{"id": attachment, "generation": float64(0), "satisfied": true}}}}
	a.csiAttachment = object{"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment", "metadata": object{"name": attachment, "uid": "dddddddd-5555-6666-7777-888888888888", "resourceVersion": "800"}, "spec": object{"attacher": "driver.longhorn.io", "nodeName": "fixture-worker", "source": object{"persistentVolumeName": name}}, "status": object{"attached": true}}
	return a, env
}

// readerRestartOriginalStore supplies the actual strict original-record representation, not a permissive fixture alias.
func readerRestartOriginalStore(t *testing.T, a *readerRestartAPI, env map[string]string) *readerCustodyStore {
	t.Helper()
	d := &recoveryDriver{i: retainedRecovery, k: readerInputKubernetes{inner: a}}
	var err error
	d.source, err = d.readSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := d.captureCustodyInputs(context.Background(), env["EMAIL_RETAINED_READER_UID"], env["EMAIL_RETAINED_CLAIM_UID"])
	if err != nil {
		t.Fatal(err)
	}
	document, err := readerCustodyDocument(bundle)
	if err != nil {
		t.Fatal(err)
	}
	record := object{"data": document, "metadata": object{"version": 1, "created_time": "2026-01-01T00:00:00Z", "deletion_time": "", "destroyed": false}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != retainedReaderDataPath {
			t.Error("original input record escaped its read-only path")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(object{"data": record})
	}))
	t.Cleanup(server.Close)
	store, err := newReaderCustodyStore(server.URL, "synthetic-private-archive-token")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	store.originalRecord, err = emailreaderinputs.RecordValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestReaderRestartGraphRefusesReplacementAndIncompleteAttachments exercises independent identity and native-state failures.
func TestReaderRestartGraphRefusesReplacementAndIncompleteAttachments(t *testing.T) {
	for _, scenario := range []string{"backend-name", "backend-namespace", "backend-replaced", "backend-unhealthy", "backend-v2", "unencrypted", "engine-namespace", "engine-name", "engine-endpoint", "engine-owner", "engine-read-only-replica", "replica-namespace", "replica-disk", "replica-inactive", "replica-replaced", "extra-replica", "attachment-name", "attachment-namespace", "attachment-generation", "extra-ticket", "csi-namespace", "duplicate-csi", "partial-engine-list", "partial-replica-list", "partial-csi-list", "malformed-continuation", "terminating-backend", "missing-version", "input-version", "source-pv-uid"} {
		t.Run(scenario, func(t *testing.T) {
			a, env := readerRestartNativeFixture(t)
			store := readerRestartOriginalStore(t, a, env)
			ids, err := authorizeReaderRestartWitness(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			attachment := text(child(a.csiAttachment, "metadata"), "name")
			switch scenario {
			case "backend-name":
				child(a.backend, "metadata")["name"] = "foreign"
			case "backend-namespace":
				child(a.backend, "metadata")["namespace"] = "foreign"
			case "backend-replaced":
				child(a.backend, "metadata")["uid"] = "aaaaaaaa-aaaa-bbbb-cccc-dddddddddddd"
			case "backend-unhealthy":
				child(a.backend, "status")["robustness"] = "degraded"
			case "backend-v2":
				child(a.backend, "spec")["dataEngine"] = "v2"
			case "unencrypted":
				child(a.backend, "spec")["encrypted"] = false
			case "engine-namespace":
				child(a.engine, "metadata")["namespace"] = "foreign"
			case "engine-name":
				child(a.engine, "metadata")["name"] = ""
			case "engine-endpoint":
				child(a.engine, "status")["endpoint"] = "/dev/mapper/foreign"
			case "engine-owner":
				items(child(a.engine, "metadata"), "ownerReferences")[0]["uid"] = "foreign"
			case "engine-read-only-replica":
				for key := range child(child(a.engine, "status"), "replicaModeMap") {
					child(child(a.engine, "status"), "replicaModeMap")[key] = "RO"
					break
				}
			case "replica-namespace":
				child(a.replicas[0], "metadata")["namespace"] = "foreign"
			case "replica-disk":
				child(a.replicas[0], "spec")["diskID"] = ""
			case "replica-inactive":
				child(a.replicas[0], "spec")["active"] = false
			case "replica-replaced":
				child(a.replicas[0], "metadata")["uid"] = "aaaaaaaa-aaaa-bbbb-cccc-dddddddddddd"
			case "extra-replica":
				a.replicas = append(a.replicas, a.replicas[0])
			case "attachment-name":
				child(a.longhornAttachment, "metadata")["name"] = "foreign"
			case "attachment-namespace":
				child(a.longhornAttachment, "metadata")["namespace"] = "foreign"
			case "attachment-generation":
				delete(child(child(child(a.longhornAttachment, "spec"), "attachmentTickets"), attachment), "generation")
				delete(child(child(child(a.longhornAttachment, "status"), "attachmentTicketStatuses"), attachment), "generation")
			case "extra-ticket":
				child(child(a.longhornAttachment, "spec"), "attachmentTickets")["foreign"] = object{}
			case "csi-namespace":
				child(a.csiAttachment, "metadata")["namespace"] = "foreign"
			case "duplicate-csi":
				a.extraCSI = []object{a.csiAttachment}
			case "partial-engine-list":
				a.continuations = map[string]any{"engines.longhorn.io": "more"}
			case "partial-replica-list":
				a.continuations = map[string]any{"replicas.longhorn.io": "more"}
			case "partial-csi-list":
				a.continuations = map[string]any{"volumeattachments.storage.k8s.io": "more"}
			case "malformed-continuation":
				a.continuations = map[string]any{"engines.longhorn.io": true}
			case "terminating-backend":
				child(a.backend, "metadata")["deletionTimestamp"] = "2026-01-01T00:00:00Z"
			case "missing-version":
				delete(child(a.engine, "metadata"), "resourceVersion")
			case "input-version":
				child(store.originalRecord, "metadata")["version"] = 2
			case "source-pv-uid":
				child(a.inner.objects[cleanupKey("PersistentVolume", "", "pvc-serving-volume")], "metadata")["uid"] = "foreign"
			}
			if _, err := captureReaderRestartDocument(context.Background(), func(k string) string { return env[k] }, a, ids, store); err == nil {
				t.Fatal("incomplete or replaced native recovery graph was accepted")
			}
			if a.mutations != 0 {
				t.Fatal("graph inspection attempted a native write")
			}
		})
	}
}

// TestReaderRestartInspectionFreezesTheWholeGraphWithoutWrites requires actual command orchestration and original input parity.
func TestReaderRestartInspectionFreezesTheWholeGraphWithoutWrites(t *testing.T) {
	a, env := readerRestartNativeFixture(t)
	d := &recoveryDriver{i: retainedRecovery, k: readerInputKubernetes{inner: a}}
	var err error
	d.source, err = d.readSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := d.captureCustodyInputs(context.Background(), env["EMAIL_RETAINED_READER_UID"], env["EMAIL_RETAINED_CLAIM_UID"])
	if err != nil {
		t.Fatal(err)
	}
	document, err := readerCustodyDocument(bundle)
	if err != nil {
		t.Fatal(err)
	}
	record := object{"data": document, "metadata": object{"version": 1, "created_time": "2026-01-01T00:00:00Z", "deletion_time": "", "destroyed": false}}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != retainedReaderDataPath {
			writes++
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(object{"data": record})
	}))
	defer server.Close()
	directory := t.TempDir()
	if os.Chmod(directory, 0700) != nil {
		t.Fatal("private fixture")
	}
	env["EMAIL_READER_INPUTS_WORK_DIR"] = directory
	before, err := json.Marshal(a.inner.objects)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := runReaderRestartWitness(context.Background(), func(k string) string { return env[k] }, a, func(context.Context, string) error { return nil }, func(func(string) string) (*readerCustodyStore, error) {
		store, err := newReaderCustodyStore(server.URL, "synthetic-private-archive-token")
		if err == nil {
			raw, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				return nil, marshalErr
			}
			store.originalRecord, err = emailreaderinputs.RecordValue(raw)
		}
		return store, err
	}, &out, false)
	raw, readErr := os.ReadFile(filepath.Join(directory, "restart-witness.json"))
	after, err := json.Marshal(a.inner.objects)
	if code != 0 || !strings.Contains(out.String(), "EMAIL_READER_RESTART_WITNESS=PREFLIGHT") || readErr != nil || len(raw) == 0 || writes != 0 || a.mutations != 0 || err != nil || !bytes.Equal(before, after) {
		t.Fatalf("inspection failed to freeze the complete graph without mutation: code=%d output=%q file=%v", code, out.String(), readErr)
	}
	var witness object
	if json.Unmarshal(raw, &witness) != nil || len(items(witness, "objects")) != 10 || !reflect.DeepEqual(child(witness, "source")["claimUID"], d.source.claimUID) || witness["readerResourceVersion"] != child(a.inner.objects[cleanupKey("Pod", retainedRecovery.namespace, "reader")], "metadata")["resourceVersion"] {
		t.Fatal("private witness omitted an attachment or source exclusion")
	}
}

// TestReaderRestartRejectsInputObjectIdentityReuse prevents an attachment from borrowing an independently retained input identity.
func TestReaderRestartRejectsInputObjectIdentityReuse(t *testing.T) {
	a, env := readerRestartNativeFixture(t)
	store := readerRestartOriginalStore(t, a, env)
	ids, err := authorizeReaderRestartWitness(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	child(a.csiAttachment, "metadata")["uid"] = child(a.inner.objects[cleanupKey("Namespace", "", retainedRecovery.namespace)], "metadata")["uid"]
	if _, err := captureReaderRestartDocument(context.Background(), func(k string) string { return env[k] }, a, ids, store); err == nil {
		t.Fatal("graph borrowed the immutable namespace input identity")
	}
}
