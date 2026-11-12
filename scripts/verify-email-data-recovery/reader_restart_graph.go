package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"regexp"
	"slices"

	"github.com/devantler-tech/platform/internal/emailreaderinputs"
	"github.com/devantler-tech/platform/internal/strictjson"
)

const restartWitnessSchema = "email-reader-restart-witness/v1"

// readerRestartKubernetes extends fixed input reads only after the restored PV has been admitted.
type readerRestartKubernetes struct {
	inner  kubernetes
	volume string
}

func (k *readerRestartKubernetes) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, errors.New("reader-restart-capability") }
	if k.inner == nil || len(input) != 0 {
		return fail()
	}
	allowed := false
	if k.volume != "" {
		for _, kind := range []string{"volumes.longhorn.io", "volumeattachments.longhorn.io"} {
			allowed = allowed || reflect.DeepEqual(args, []string{"get", kind, k.volume, "--ignore-not-found=true", "-o=json", "--namespace=longhorn-system"})
		}
		for _, kind := range []string{"engines.longhorn.io", "replicas.longhorn.io"} {
			allowed = allowed || reflect.DeepEqual(args, []string{"get", kind, "--selector=longhornvolume=" + k.volume, "-o=json", "--namespace=longhorn-system"})
		}
		allowed = allowed || reflect.DeepEqual(args, []string{"get", "volumeattachments.storage.k8s.io", "-o=json"})
	}
	var raw []byte
	var err error
	if allowed {
		raw, err = k.inner.call(ctx, nil, args...)
	} else {
		raw, err = (readerInputKubernetes{inner: k.inner}).call(ctx, nil, args...)
	}
	if err != nil || len(raw) > 4<<20 || (len(bytes.TrimSpace(raw)) != 0 && strictjson.Validate(raw, nil) != nil) {
		return fail()
	}
	return raw, nil
}

// restartResourceList refuses incomplete native pagination and ambiguous list envelopes.
func restartResourceList(ctx context.Context, k kubernetes, apiVersion, kind string, args ...string) ([]object, error) {
	raw, err := k.call(ctx, nil, args...)
	var list object
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &list) != nil || text(list, "apiVersion") != apiVersion || text(list, "kind") != kind+"List" || text(child(list, "metadata"), "continue") != "" {
		return nil, errors.New("reader-restart-list")
	}
	if token, present := child(list, "metadata")["continue"]; present && token != "" {
		return nil, errors.New("reader-restart-list")
	}
	array, ok := list["items"].([]any)
	if !ok || len(array) > 512 {
		return nil, errors.New("reader-restart-list")
	}
	result := make([]object, 0, len(array))
	for _, value := range array {
		o, ok := value.(object)
		if !ok || text(o, "apiVersion") != apiVersion || text(o, "kind") != kind {
			return nil, errors.New("reader-restart-list")
		}
		result = append(result, o)
	}
	return result, nil
}

// restartOwnedByVolume requires the exact original backend controller identity.
func restartOwnedByVolume(o object, volume, uid string) bool {
	owners := items(child(o, "metadata"), "ownerReferences")
	return len(owners) == 1 && text(owners[0], "apiVersion") == "longhorn.io/v1beta2" && text(owners[0], "kind") == "Volume" && text(owners[0], "name") == volume && text(owners[0], "uid") == uid
}

// freezeRestartResource retains full definitions and stable identity fields, never volatile heartbeat times.
func freezeRestartResource(o, state object) object {
	m := child(o, "metadata")
	metadata := object{}
	for _, key := range []string{"name", "namespace", "uid", "labels", "annotations", "ownerReferences", "finalizers"} {
		if value, present := m[key]; present {
			metadata[key] = value
		}
	}
	return object{"apiVersion": o["apiVersion"], "kind": o["kind"], "metadata": metadata, "spec": o["spec"], "state": state}
}

// captureReaderRestartDocument links complete native storage definitions to the original immutable input record.
func captureReaderRestartDocument(ctx context.Context, env func(string) string, k kubernetes, ids readerRestartIDs, store *readerCustodyStore) (object, error) {
	fail := func() (object, error) { return nil, errors.New("reader-restart-graph") }
	if store == nil || store.originalRecord == nil {
		return fail()
	}
	raw, err := json.Marshal(store.originalRecord)
	if err != nil {
		return fail()
	}
	if _, err = emailreaderinputs.RecordValue(raw); err != nil {
		return fail()
	}
	inputHash := sha256.Sum256(raw)
	native := &readerRestartKubernetes{inner: k}
	d := &recoveryDriver{i: retainedRecovery, k: native}
	d.source, err = d.readSource(ctx)
	if err != nil {
		return fail()
	}
	core, err := d.readRetainedVolume(ctx, ids.Volume)
	if err != nil || text(child(core[2], "spec"), "persistentVolumeReclaimPolicy") != "Retain" {
		return fail()
	}
	bundle, err := d.captureCustodyInputs(ctx, ids.Volume.ReaderUID, ids.Volume.ClaimUID)
	if err != nil {
		return fail()
	}
	expected, err := readerCustodyDocument(bundle)
	if err != nil {
		return fail()
	}
	matched, err := store.matchesOriginalInputs(ctx, expected)
	if err != nil || !matched || d.sourceRetained(ctx) != nil {
		return fail()
	}
	name := text(child(core[2], "metadata"), "name")
	node := text(child(core[0], "spec"), "nodeName")
	if !kubernetesName.MatchString(node) {
		return fail()
	}
	native.volume = name
	backend, found, err := d.lookup(ctx, "volumes.longhorn.io", name, "longhorn-system")
	if err != nil || !found || text(backend, "apiVersion") != "longhorn.io/v1beta2" || text(backend, "kind") != "Volume" || text(child(backend, "metadata"), "uid") != ids.Backend || text(child(backend, "metadata"), "name") != name {
		return fail()
	}
	bs, status := child(backend, "spec"), child(backend, "status")
	if bs["encrypted"] != true || bs["numberOfReplicas"] != float64(3) || text(bs, "dataEngine") != "v1" || text(bs, "frontend") != "blockdev" || text(bs, "nodeID") != node || text(status, "currentNodeID") != node || text(status, "state") != "attached" || text(status, "robustness") != "healthy" {
		return fail()
	}
	engines, err := restartResourceList(ctx, native, "longhorn.io/v1beta2", "Engine", "get", "engines.longhorn.io", "--selector=longhornvolume="+name, "-o=json", "--namespace=longhorn-system")
	if err != nil || len(engines) != 1 {
		return fail()
	}
	engine := engines[0]
	es, est := child(engine, "spec"), child(engine, "status")
	if !restartOwnedByVolume(engine, name, ids.Backend) || text(es, "volumeName") != name || es["active"] != true || text(es, "nodeID") != node || text(es, "frontend") != "blockdev" || text(es, "dataEngine") != "v1" || text(est, "currentState") != "running" || text(est, "endpoint") != "/dev/longhorn/"+name {
		return fail()
	}
	replicas, err := restartResourceList(ctx, native, "longhorn.io/v1beta2", "Replica", "get", "replicas.longhorn.io", "--selector=longhornvolume="+name, "-o=json", "--namespace=longhorn-system")
	if err != nil || len(replicas) != 3 || len(child(est, "replicaModeMap")) != 3 {
		return fail()
	}
	slices.SortFunc(replicas, func(a, b object) int {
		return bytes.Compare([]byte(text(child(a, "metadata"), "name")), []byte(text(child(b, "metadata"), "name")))
	})
	var replicaUIDs []string
	for _, replica := range replicas {
		rs, rm := child(replica, "spec"), child(replica, "metadata")
		if !restartOwnedByVolume(replica, name, ids.Backend) || text(rs, "volumeName") != name || rs["active"] != true || text(rs, "dataEngine") != "v1" || text(rs, "failedAt") != "" || !kubernetesName.MatchString(text(rs, "nodeID")) || text(rs, "diskID") == "" || text(rs, "dataDirectoryName") == "" || text(child(replica, "status"), "currentState") != "running" || child(est, "replicaModeMap")[text(rm, "name")] != "RW" {
			return fail()
		}
		replicaUIDs = append(replicaUIDs, text(rm, "uid"))
	}
	slices.Sort(replicaUIDs)
	if !slices.Equal(replicaUIDs, ids.Replicas) {
		return fail()
	}
	longhornAttachment, found, err := d.lookup(ctx, "volumeattachments.longhorn.io", name, "longhorn-system")
	if err != nil || !found || text(longhornAttachment, "apiVersion") != "longhorn.io/v1beta2" || text(longhornAttachment, "kind") != "VolumeAttachment" || text(child(longhornAttachment, "metadata"), "name") != name || !restartOwnedByVolume(longhornAttachment, name, ids.Backend) || text(child(longhornAttachment, "spec"), "volume") != name {
		return fail()
	}
	attachments, err := restartResourceList(ctx, native, "storage.k8s.io/v1", "VolumeAttachment", "get", "volumeattachments.storage.k8s.io", "-o=json")
	if err != nil {
		return fail()
	}
	var attachment object
	for _, candidate := range attachments {
		if text(child(child(candidate, "spec"), "source"), "persistentVolumeName") != name {
			continue
		}
		if attachment != nil {
			return fail()
		}
		attachment = candidate
	}
	if attachment == nil {
		return fail()
	}
	cs, cst, cm := child(attachment, "spec"), child(attachment, "status"), child(attachment, "metadata")
	attachmentName := text(cm, "name")
	if !regexp.MustCompile(`^csi-[0-9a-f]{64}$`).MatchString(attachmentName) || text(cs, "attacher") != "driver.longhorn.io" || text(cs, "nodeName") != node || child(cs, "source")["inlineVolumeSpec"] != nil || cst["attached"] != true || cst["attachError"] != nil {
		return fail()
	}
	tickets := child(child(longhornAttachment, "spec"), "attachmentTickets")
	ticketStates := child(child(longhornAttachment, "status"), "attachmentTicketStatuses")
	ticket, ticketState := child(tickets, attachmentName), child(ticketStates, attachmentName)
	generation, hasGeneration := ticket["generation"].(float64)
	if !hasGeneration || generation < 0 || generation > 1<<53-1 || math.Trunc(generation) != generation || len(tickets) != 1 || len(ticketStates) != 1 || text(ticket, "id") != attachmentName || text(ticket, "type") != "csi-attacher" || text(ticket, "nodeID") != node || text(child(ticket, "parameters"), "disableFrontend") != "false" || text(ticketState, "id") != attachmentName || ticketState["satisfied"] != true || !reflect.DeepEqual(ticket["generation"], ticketState["generation"]) {
		return fail()
	}
	objects := []object{
		freezeRestartResource(core[0], object{"phase": "Running", "ready": true}),
		freezeRestartResource(core[1], object{"phase": "Bound"}),
		freezeRestartResource(core[2], object{"phase": "Bound"}),
		freezeRestartResource(backend, object{"state": "attached", "robustness": "healthy", "currentNodeID": node}),
		freezeRestartResource(engine, object{"currentState": "running", "endpoint": est["endpoint"], "replicaModeMap": est["replicaModeMap"]}),
		freezeRestartResource(longhornAttachment, object{"ticket": object{"id": attachmentName, "generation": ticketState["generation"], "satisfied": true}}),
	}
	for _, replica := range replicas {
		objects = append(objects, freezeRestartResource(replica, object{"currentState": "running"}))
	}
	objects = append(objects, freezeRestartResource(attachment, object{"attached": true, "attachmentMetadata": cst["attachmentMetadata"]}))
	seen := map[string]bool{}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	rv := regexp.MustCompile(`^[1-9][0-9]{0,31}$`)
	for _, uid := range []string{d.source.podUID, d.source.claimUID, d.source.pvUID} {
		if !uuid.MatchString(uid) || seen[uid] {
			return fail()
		}
		seen[uid] = true
	}
	for _, input := range bundle.Inputs {
		if input.Kind == "Pod" || input.Kind == "PersistentVolumeClaim" {
			continue
		}
		if seen[input.UID] {
			return fail()
		}
		seen[input.UID] = true
	}
	originals := append(append(append([]object{}, core...), backend, engine, longhornAttachment), replicas...)
	originals = append(originals, attachment)
	for _, o := range originals {
		m := child(o, "metadata")
		uid := text(m, "uid")
		validName := kubernetesName.MatchString(text(m, "name"))
		if text(o, "apiVersion") == "storage.k8s.io/v1" {
			validName = text(m, "name") == attachmentName
		}
		if !validName || !uuid.MatchString(uid) || seen[uid] || !rv.MatchString(text(m, "resourceVersion")) || m["deletionTimestamp"] != nil || child(o, "spec") == nil {
			return fail()
		}
		if text(o, "apiVersion") == "longhorn.io/v1beta2" && text(m, "namespace") != "longhorn-system" || text(o, "apiVersion") == "storage.k8s.io/v1" && text(m, "namespace") != "" {
			return fail()
		}
		seen[uid] = true
	}
	result := object{"schema": restartWitnessSchema, "trialRun": retainedRecovery.run, "trialRevision": retainedRecovery.revision,
		"captureRun": env("GITHUB_RUN_ID"), "captureRevision": env("GITHUB_SHA"), "inputsRecordSHA256": hex.EncodeToString(inputHash[:]), "readerResourceVersion": text(child(core[0], "metadata"), "resourceVersion"),
		"source": object{"podUID": d.source.podUID, "claimUID": d.source.claimUID, "pvUID": d.source.pvUID, "volume": d.source.pv}, "objects": objects}
	raw, err = json.Marshal(result)
	var frozen object
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &frozen) != nil {
		return fail()
	}
	return frozen, nil
}
