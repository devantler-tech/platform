package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
)

type hostMemoryReboot struct {
	c                     client
	target, source, owner string
	pools                 map[string]migrationPool
	inspect               func(context.Context) (hostMemoryReport, error)
}

// authorizeHostReboot requires a separate confirmation for the fixed permanent
// worker selection, then applies the shared protected-workflow admission gate.
func authorizeHostReboot(getenv func(string) string, target string) error {
	if !inventoryTarget.MatchString(target) || getenv("LONGHORN_V2_NODE") != target || getenv("LONGHORN_V2_OPERATION") != "reboot-host-for-hugepages" {
		return errHostMemory
	}
	return authorizeWorkflow(getenv, "reboot-longhorn-v2-host-"+target)
}

type hostRebootRecord struct {
	Version          int                             `json:"version"`
	Target           string                          `json:"target"`
	Source           string                          `json:"source"`
	OwnerRun         string                          `json:"ownerRun"`
	Phase            string                          `json:"phase"`
	NodeUID          string                          `json:"nodeUID"`
	ProviderID       string                          `json:"providerID"`
	MachineUUID      string                          `json:"machineUUID"`
	LonghornUID      string                          `json:"longhornUID"`
	EphemeralUUID    string                          `json:"ephemeralUUID"`
	OriginalDiskUUID string                          `json:"originalDiskUUID"`
	OriginalBootID   string                          `json:"originalBootID"`
	RecoveredBootID  string                          `json:"recoveredBootID"`
	OSImage          string                          `json:"osImage"`
	Architecture     string                          `json:"architecture"`
	OriginalDisks    object                          `json:"originalDisks"`
	Databases        map[string]hostDatabaseIdentity `json:"databases"`
}

type hostDatabaseIdentity struct{ UID, SystemID string }

var hostSystemID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

var errHostDatabasesRecovering = errors.New("the original database quorum is still recovering after eviction")

// identity supplies the existing worker validator with immutable bindings; the
// reboot record retains its own operation phases and storage-preservation proof.
func (r hostRebootRecord) identity() journal {
	return journal{Schema: 1, Target: r.Target, Source: r.Source, OwnerRun: r.OwnerRun, Phase: "draining", NodeUID: r.NodeUID, ProviderID: r.ProviderID,
		MachineUUID: r.MachineUUID, LonghornUID: r.LonghornUID, EphemeralUUID: r.EphemeralUUID, OriginalDisks: r.OriginalDisks}
}

// validate rejects incomplete or out-of-profile records before their phase can authorize
// an action, including incomplete original storage and database identities.
func (r hostRebootRecord) validate(target string) error {
	if r.Version != 1 || validateJournal(r.identity(), target) != nil || r.OriginalDiskUUID == "" || !inventoryBoot.MatchString(r.OriginalBootID) || r.OSImage == "" || r.Architecture != "amd64" || len(r.Databases) != 3 {
		return errHostMemory
	}
	for _, key := range []string{"observability/coroot-db", "umami/umami-db", "wedding-app/wedding-db"} {
		if r.Databases[key].UID == "" || !hostSystemID.MatchString(r.Databases[key].SystemID) {
			return errHostMemory
		}
	}
	switch r.Phase {
	case "draining", "reboot-requested":
		if r.RecoveredBootID != "" {
			return errHostMemory
		}
	case "recovered":
		if !inventoryBoot.MatchString(r.RecoveredBootID) || r.RecoveredBootID == r.OriginalBootID {
			return errHostMemory
		}
	default:
		return errHostMemory
	}
	return nil
}

// hostRegisteredDiskUUID accepts the single registered V1 filesystem disk.
// Recording its identity separately detects replacement behind an unchanged path.
func hostRegisteredDiskUUID(storage object) string {
	disks, ok := value(storage, "spec", "disks").(object)
	statuses, statusOK := value(storage, "status", "diskStatus").(object)
	if !ok || !statusOK || len(disks) != 1 || len(statuses) != 1 {
		return ""
	}
	for name, raw := range disks {
		disk, diskOK := raw.(object)
		status, statusOK := statuses[name].(object)
		if !diskOK || !statusOK || text(disk, "path") != "/var/lib/longhorn" || text(disk, "diskType") != "filesystem" ||
			text(status, "diskPath") != "/var/lib/longhorn" || text(status, "diskType") != "filesystem" {
			return ""
		}
		return text(status, "diskUUID")
	}
	return ""
}

func (h hostMemoryReboot) name() string { return "longhorn-v2-host-memory-" + h.target }

// peersClear prevents a new disruption while another worker has an unfinished
// request or has lost its recovery proof. Recovery of this worker stays possible.
func (h hostMemoryReboot) peersClear(ctx context.Context, nodes []object) error {
	byName, err := indexed(nodes)
	if err != nil {
		return errHostMemory
	}
	for _, target := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		if target == h.target {
			continue
		}
		n := byName[target]
		if n == nil || value(n, "spec", "unschedulable") == true || text(n, "metadata", "annotations", fenceAnnotation) != "" {
			return errors.New("another worker has an outstanding maintenance cordon")
		}
		peer := h
		peer.target = target
		cm, r, err := peer.read(ctx)
		if err != nil || cm != nil && r.Phase != "recovered" {
			return errors.New("another worker has unfinished or unreadable host memory maintenance")
		}
		if cm != nil {
			if _, err = verifyNode(n, r.identity(), true); err != nil || text(n, "status", "nodeInfo", "bootID") != r.RecoveredBootID || !hugepagesReady(n) {
				return errors.New("another worker's recorded recovery is no longer proven")
			}
		}
	}
	return nil
}

// read accepts only the fixed journal object and a singular, validated record.
// A malformed or mis-scoped response cannot supply a later patch destination.
func (h hostMemoryReboot) read(ctx context.Context) (object, hostRebootRecord, error) {
	cm, err := h.c.get(ctx, "configmap", "longhorn-system", h.name(), true)
	if err != nil || cm == nil {
		return cm, hostRebootRecord{}, err
	}
	if text(cm, "metadata", "name") != h.name() || text(cm, "metadata", "namespace") != "longhorn-system" {
		return nil, hostRebootRecord{}, errHostMemory
	}
	encoded := text(cm, "data", "record")
	if len(encoded) == 0 || len(encoded) > 64<<10 {
		return nil, hostRebootRecord{}, errHostMemory
	}
	d := json.NewDecoder(bytes.NewBufferString(encoded))
	d.DisallowUnknownFields()
	var r hostRebootRecord
	var extra any
	if d.Decode(&r) != nil || d.Decode(&extra) != io.EOF || r.validate(h.target) != nil {
		return nil, hostRebootRecord{}, errHostMemory
	}
	return cm, r, nil
}

// write persists intent only for the current reviewed source and run. Existing
// records retain the API object's UID and resource-version preconditions.
func (h hostMemoryReboot) write(ctx context.Context, cm object, r hostRebootRecord) error {
	if cm != nil && (text(cm, "metadata", "name") != h.name() || text(cm, "metadata", "namespace") != "longhorn-system") {
		return errHostMemory
	}
	if r.validate(h.target) != nil || r.Source != h.source || r.OwnerRun != h.owner {
		return errHostMemory
	}
	if err := sourceCurrent(ctx, h.c.run, h.source); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return errHostMemory
	}
	if cm != nil {
		return h.c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(b)})
	}
	body, err := json.Marshal(object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": h.name(), "namespace": "longhorn-system"}, "data": object{"record": string(b)}})
	if err != nil {
		return errHostMemory
	}
	_, err = h.c.kubectl(ctx, body, "create", "--filename=/dev/stdin", "--output=name")
	return err
}

// owned rebinds the complete journal and current main immediately before a
// worker mutation, so an intervening takeover or state change holds the action.
func (h hostMemoryReboot) owned(ctx context.Context, expected hostRebootRecord) (object, error) {
	cm, actual, err := h.read(ctx)
	if err != nil || cm == nil || !reflect.DeepEqual(expected, actual) || actual.OwnerRun != h.owner || actual.Source != h.source {
		return nil, errHostMemory
	}
	return cm, sourceCurrent(ctx, h.c.run, h.source)
}

// databases requires every declared database cluster to be fully healthy and
// preserves its cluster and system identities across the maintenance operation.
func (h hostMemoryReboot) databases(ctx context.Context, expected map[string]hostDatabaseIdentity) (map[string]hostDatabaseIdentity, error) {
	items, err := h.c.list(ctx, "clusters.postgresql.cnpg.io", "*")
	if err != nil || len(items) != 3 {
		return nil, errHostMemory
	}
	result := map[string]hostDatabaseIdentity{}
	recovering := false
	for _, db := range items {
		key := text(db, "metadata", "namespace") + "/" + text(db, "metadata", "name")
		if key != "observability/coroot-db" && key != "umami/umami-db" && key != "wedding-app/wedding-db" || result[key].UID != "" ||
			text(db, "metadata", "uid") == "" || text(db, "metadata", "resourceVersion") == "" || value(db, "metadata", "deletionTimestamp") != nil ||
			value(db, "spec", "instances") != float64(3) || !hostSystemID.MatchString(text(db, "status", "systemID")) {
			return nil, errHostMemory
		}
		result[key] = hostDatabaseIdentity{UID: text(db, "metadata", "uid"), SystemID: text(db, "status", "systemID")}
		ready := value(db, "status", "readyInstances")
		phase := text(db, "status", "phase")
		if (ready != float64(2) && ready != float64(3)) || phase == "" {
			return nil, errHostMemory
		}
		recovering = recovering || ready != float64(3) || phase != "Cluster in healthy state"
	}
	if expected != nil && !reflect.DeepEqual(result, expected) {
		return nil, errHostMemory
	}
	if recovering {
		return nil, errHostDatabasesRecovering
	}
	return result, nil
}

// waitDrainRecovery gives an unchanged database quorum a bounded startup window
// after eviction. Changed ownership, source, target identity, peer maintenance or
// other safety evidence fails immediately; the final two fresh checks still run.
func (h hostMemoryReboot) waitDrainRecovery(ctx context.Context, r hostRebootRecord) error {
	return waitReadyWithin(ctx, 20*time.Minute, func(ctx context.Context) error {
		if _, err := h.owned(ctx, r); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		n, _, err := h.targetState(ctx, r)
		if err != nil || text(n, "status", "nodeInfo", "bootID") != r.OriginalBootID || value(n, "spec", "unschedulable") != true || text(n, "metadata", "annotations", fenceAnnotation) != h.name() {
			return errors.Join(errReadinessTerminal, errHostMemory)
		}
		s, _, err := h.healthy(ctx, r.Databases)
		if err != nil && !errors.Is(err, errHostDatabasesRecovering) {
			return errors.Join(errReadinessTerminal, err)
		}
		if peerErr := h.peersClear(ctx, s.nodes); peerErr != nil {
			return errors.Join(errReadinessTerminal, peerErr)
		}
		return err
	})
}

// healthy gates this pre-V2 operation on the full migration roster, complete V1
// redundancy and database health. Every call collects a fresh API inventory.
func (h hostMemoryReboot) healthy(ctx context.Context, expected map[string]hostDatabaseIdentity) (inventory, map[string]hostDatabaseIdentity, error) {
	setting, err := h.c.get(ctx, "settings.longhorn.io", "longhorn-system", "v2-data-engine", false)
	if err != nil || text(setting, "metadata", "name") != "v2-data-engine" || text(setting, "metadata", "namespace") != "longhorn-system" || text(setting, "value") != "false" {
		return inventory{}, nil, errors.New("host memory maintenance requires V2 to remain disabled")
	}
	s, err := h.c.snapshot(ctx)
	if err != nil {
		return inventory{}, nil, err
	}
	if verifyCapacityRoster(s.nodes, h.pools, false) != nil || verifyRedundancy(s) != nil {
		return inventory{}, nil, errHostMemory
	}
	for _, v := range s.volumes {
		if text(v, "spec", "dataEngine") != "v1" {
			return inventory{}, nil, errHostMemory
		}
	}
	databases, err := h.databases(ctx, expected)
	return s, databases, err
}

// targetState binds the live endpoint to the original hardware, registered disk
// and encrypted filesystem. Only this operation's cordon may accompany recovery.
func (h hostMemoryReboot) targetState(ctx context.Context, r hostRebootRecord) (object, string, error) {
	n, err := h.c.get(ctx, "node", "", h.target, false)
	if err != nil {
		return nil, "", err
	}
	address, err := verifyNode(n, r.identity(), true)
	status, _ := value(n, "status").(map[string]any)
	fence := text(n, "metadata", "annotations", fenceAnnotation)
	if err != nil || !ready(status) || !workerPressureClear(n) || fence != "" && fence != h.name() || value(n, "spec", "unschedulable") == true && fence != h.name() ||
		text(n, "status", "nodeInfo", "osImage") != r.OSImage || text(n, "status", "nodeInfo", "architecture") != r.Architecture || h.c.verifyHost(ctx, address, n) != nil {
		return nil, "", errHostMemory
	}
	storage, err := h.c.get(ctx, "nodes.longhorn.io", "longhorn-system", h.target, false)
	if err != nil || text(storage, "metadata", "name") != h.target || text(storage, "metadata", "namespace") != "longhorn-system" || text(storage, "metadata", "uid") != r.LonghornUID || hostRegisteredDiskUUID(storage) != r.OriginalDiskUUID || !reflect.DeepEqual(value(storage, "spec", "disks"), r.OriginalDisks) ||
		value(storage, "spec", "allowScheduling") != true || value(storage, "spec", "evictionRequested") != false {
		return nil, "", errHostMemory
	}
	volume, err := h.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil || text(volume, "spec", "uuid") != r.EphemeralUUID || text(volume, "spec", "phase") != "ready" || text(volume, "spec", "encryptionProvider") != "luks2" {
		return nil, "", errHostMemory
	}
	setting, err := h.c.talosResource(ctx, address, "kernelparamspec", "proc.sys.vm.nr_hugepages")
	if err != nil || text(setting, "metadata", "namespace") != "runtime" || text(setting, "spec", "value") != "1024" {
		return nil, "", errHostMemory
	}
	return n, address, nil
}

// memory uses the native observer's unchanged API, hardware and boot bracket.
// The injected inspection boundary lets flow tests exercise recovery deadlines.
func (h hostMemoryReboot) memory(ctx context.Context) (hostMemoryReport, error) {
	if h.inspect != nil {
		return h.inspect(ctx)
	}
	receipt, err := observeHostMemory(ctx, h.c, h.target, newInventoryReader)
	return receipt.Report, err
}

// initialize records an unused allocation shortfall and original identities
// before any cordon or eviction. A completed or busy allocation cannot start it.
func (h hostMemoryReboot) initialize(ctx context.Context) (hostRebootRecord, error) {
	s, databases, err := h.healthy(ctx, nil)
	if err != nil {
		return hostRebootRecord{}, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return hostRebootRecord{}, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return hostRebootRecord{}, err
	}
	n, lh := nodes[h.target], storage[h.target]
	r := hostRebootRecord{Version: 1, Target: h.target, Source: h.source, OwnerRun: h.owner, Phase: "draining", NodeUID: text(n, "metadata", "uid"), ProviderID: text(n, "spec", "providerID"),
		MachineUUID: text(n, "status", "nodeInfo", "systemUUID"), OriginalBootID: text(n, "status", "nodeInfo", "bootID"), LonghornUID: text(lh, "metadata", "uid"),
		OSImage: text(n, "status", "nodeInfo", "osImage"), Architecture: text(n, "status", "nodeInfo", "architecture"), Databases: databases, OriginalDiskUUID: hostRegisteredDiskUUID(lh)}
	r.OriginalDisks, _ = value(lh, "spec", "disks").(object)
	address, err := verifyNode(n, r.identity(), true)
	if err != nil || value(n, "spec", "unschedulable") == true || text(n, "metadata", "annotations", fenceAnnotation) != "" {
		return hostRebootRecord{}, errHostMemory
	}
	eph, err := h.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil {
		return hostRebootRecord{}, err
	}
	r.EphemeralUUID = text(eph, "spec", "uuid")
	if r.validate(h.target) != nil {
		return hostRebootRecord{}, errHostMemory
	}
	if _, _, err = h.targetState(ctx, r); err != nil {
		return hostRebootRecord{}, err
	}
	memory, err := h.memory(ctx)
	if err != nil || memory.PersistentPages >= 1024 || memory.TotalPages != memory.PersistentPages || memory.FreePages != memory.TotalPages || memory.PageSizeKiB != 2048 || memory.ReservedPages != 0 || memory.SurplusPages != 0 {
		return hostRebootRecord{}, errors.New("native allocation no longer proves an unused huge-page shortfall; inspect host memory before a reboot")
	}
	// A refused dispatch must not leave a second unfinished journal that would
	// prevent the original worker from resuming its own maintenance.
	if err = h.peersClear(ctx, s.nodes); err != nil {
		return hostRebootRecord{}, err
	}
	return r, h.write(ctx, nil, r)
}

// run coordinates one journaled reboot and proves recovery before releasing its
// cordon. A saved reboot-requested phase resumes observation without redelivery.
func (h hostMemoryReboot) run(ctx context.Context) error {
	if !inventoryTarget.MatchString(h.target) {
		return errHostMemory
	}
	if err := sourceCurrent(ctx, h.c.run, h.source); err != nil {
		return err
	}
	cm, r, err := h.read(ctx)
	if err != nil {
		return err
	}
	if cm == nil {
		r, err = h.initialize(ctx)
		if err != nil {
			return err
		}
	} else if r.OwnerRun != h.owner || r.Source != h.source {
		if previousRunCompleted(ctx, h.c, r.OwnerRun, r.Source) != nil {
			return errHostMemory
		}
		r.OwnerRun, r.Source = h.owner, h.source
		if err = h.write(ctx, cm, r); err != nil {
			return err
		}
	}
	if r.Phase == "draining" {
		s, _, err := h.healthy(ctx, r.Databases)
		if err != nil {
			return err
		}
		if err = h.peersClear(ctx, s.nodes); err != nil {
			return err
		}
		n, address, err := h.targetState(ctx, r)
		if err != nil || text(n, "status", "nodeInfo", "bootID") != r.OriginalBootID {
			return errHostMemory
		}
		if _, err = h.owned(ctx, r); err != nil {
			return err
		}
		changes := append(annotationPatch(n, h.name()), object{"op": "add", "path": "/spec/unschedulable", "value": true})
		if err = h.c.patch(ctx, "node", "", n, changes...); err != nil {
			return err
		}
		if _, err = h.owned(ctx, r); err != nil {
			return err
		}
		if _, err = h.c.kubectl(ctx, nil, "drain", h.target, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=40m"); err != nil {
			return errors.New("host memory maintenance drain was blocked; no reboot was requested")
		}
		if err = h.waitDrainRecovery(ctx, r); err != nil {
			return err
		}
		// Keep the existing full redundancy floor: no request while a copy or
		// database is still recovering from the eviction.
		for range 2 {
			s, _, err := h.healthy(ctx, r.Databases)
			if err != nil {
				return err
			}
			if err = h.peersClear(ctx, s.nodes); err != nil {
				return err
			}
			n, address, err = h.targetState(ctx, r)
			if err != nil || text(n, "status", "nodeInfo", "bootID") != r.OriginalBootID || value(n, "spec", "unschedulable") != true || text(n, "metadata", "annotations", fenceAnnotation) != h.name() {
				return errHostMemory
			}
		}
		cm, err = h.owned(ctx, r)
		if err != nil {
			return err
		}
		r.Phase = "reboot-requested"
		if err = h.write(ctx, cm, r); err != nil {
			return err
		}
		// The durable intent precedes the only request. An uncertain response
		// is never retried: recovery must prove a new boot and intact storage.
		_, _ = h.c.talosCommand(ctx, 15*time.Minute, address, "reboot", "--wait", "--timeout=15m")
	}
	recovery, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	var recovered object
	if err = waitReadyWithin(recovery, 20*time.Minute, func(ctx context.Context) error {
		n, _, err := h.targetState(ctx, r)
		boot := text(n, "status", "nodeInfo", "bootID")
		if err != nil || !inventoryBoot.MatchString(boot) || boot == r.OriginalBootID || r.RecoveredBootID != "" && boot != r.RecoveredBootID || !hugepagesReady(n) {
			return errHostMemory
		}
		memory, err := h.memory(ctx)
		if err != nil || memory.PersistentPages != 1024 || memory.TotalPages != 1024 || memory.PageSizeKiB != 2048 {
			return errHostMemory
		}
		if _, _, err = h.healthy(ctx, r.Databases); err != nil {
			return err
		}
		recovered = n
		return nil
	}); err != nil {
		return err
	}
	if _, err = h.owned(ctx, r); err != nil {
		return err
	}
	if text(recovered, "metadata", "annotations", fenceAnnotation) == h.name() {
		if err = h.c.patch(ctx, "node", "", recovered, object{"op": "add", "path": "/spec/unschedulable", "value": false}, object{"op": "remove", "path": "/metadata/annotations/" + strings.ReplaceAll(fenceAnnotation, "/", "~1")}); err != nil {
			return err
		}
	}
	released, _, err := h.targetState(ctx, r)
	if err != nil || value(released, "spec", "unschedulable") == true || text(released, "metadata", "annotations", fenceAnnotation) != "" || text(released, "status", "nodeInfo", "bootID") != text(recovered, "status", "nodeInfo", "bootID") {
		return errHostMemory
	}
	if r.Phase == "recovered" {
		return nil
	}
	cm, err = h.owned(ctx, r)
	if err != nil {
		return err
	}
	r.Phase, r.RecoveredBootID = "recovered", text(recovered, "status", "nodeInfo", "bootID")
	return h.write(ctx, cm, r)
}
