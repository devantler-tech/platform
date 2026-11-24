package main

import (
	"net/netip"
	"reflect"
	"time"
)

type nativeRestoreAttachedObservation struct {
	Claim, Class, PV, Pod object
	Inventory             inventory
	Frontends             []object
	Managers              map[string]nativeRestoreV2ManagerIdentity
	Config                []byte
	HelperDigest          string
}

type nativeRestoreAttachmentReceipt struct {
	ConsumerContainerID, ConsumerStartedAt       string
	Consumer, Engine, Frontend                   benchmarkResource
	Replicas                                     [3]benchmarkResource
	ConsumerNode, EngineNode, TargetIP, Endpoint string
	TargetPort                                   int
}

func verifyNativeRestoreAttached(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, observed nativeRestoreAttachedObservation) (nativeRestoreAttachmentReceipt, error) {
	fail := func() (nativeRestoreAttachmentReceipt, error) {
		return nativeRestoreAttachmentReceipt{}, errNativeRestoreEvidence
	}
	var volume object
	for _, candidate := range observed.Inventory.volumes {
		if benchmarkIdentity(candidate) == state.Resources["LonghornVolume"] {
			if volume != nil {
				return fail()
			}
			volume = candidate
		}
	}
	if verifyNativeRestoreStorage(record, seed, state, observed.Claim, observed.Class, observed.PV, volume) != nil ||
		verifyNativeRestoreConsumer(record, seed, state, observed.Pod, observed.HelperDigest) != nil || verifyRedundancy(observed.Inventory) != nil || observed.Frontends == nil {
		return fail()
	}
	process := value(observed.Pod, "status", "containerStatuses").([]any)[0].(object)
	started, err := time.Parse(time.RFC3339Nano, text(process, "state", "running", "startedAt"))
	if err != nil || started.IsZero() || started.After(time.Now()) || !benchmarkContainerID.MatchString(text(process, "containerID")) {
		return fail()
	}
	targets, err := qualifyNativeRestoreTargets(state.Targets, observed.Inventory.nodes, observed.Inventory.storageNodes, "longhorn-native-v2-"+state.Run, true, observed.Config)
	consumer := text(observed.Pod, "spec", "nodeName")
	frontend := text(volume, "spec", "frontend")
	if err != nil || targets[consumer].Node.UID == "" || text(volume, "status", "state") != "attached" || text(volume, "status", "robustness") != "healthy" ||
		text(volume, "spec", "nodeID") != consumer || text(volume, "status", "currentNodeID") != consumer ||
		value(volume, "status", "expansionRequired") == true ||
		(frontend != "blockdev" && frontend != "ublk") || value(volume, "spec", "disableFrontend") == true || value(volume, "status", "frontendDisabled") != false ||
		!absentOrEmptyString(value(volume, "spec", "migrationNodeID")) || !absentOrEmptyString(value(volume, "status", "currentMigrationNodeID")) || !absentOrEmptyString(value(volume, "status", "switchoverState")) {
		return fail()
	}
	used := nativeRestoreReceiptUIDs(record, seed, state)
	if !nativeRestoreAttachedManagerRoster(state.Targets, observed.Managers, used) {
		return fail()
	}
	engine, err := nativeRestoreCurrentInstance(observed.Inventory.engines, "Engine", volume, used)
	engineNode := text(engine, "spec", "nodeID")
	if err != nil || targets[engineNode].Node.UID == "" || !nativeRestoreInstanceOnManager(engine, observed.Managers) || text(volume, "status", "currentEngineNodeID") != engineNode ||
		text(engine, "spec", "volumeSize") != text(volume, "spec", "size") || text(engine, "status", "currentSize") != text(volume, "spec", "size") || value(engine, "status", "isExpanding") == true {
		return fail()
	}
	address := text(engine, "status", "storageIP")
	ip, err := netip.ParseAddr(address)
	port, ok := value(engine, "status", "port").(float64)
	if err != nil || !ip.IsGlobalUnicast() || !ok || !(port > 0 && port <= 65535) || port != float64(int(port)) {
		return fail()
	}
	initiator, err := nativeRestoreCurrentInstance(observed.Frontends, "EngineFrontend", volume, used)
	if err != nil || text(initiator, "spec", "nodeID") != consumer || !nativeRestoreInstanceOnManager(initiator, observed.Managers) || text(initiator, "spec", "engineName") != text(engine, "metadata", "name") ||
		text(initiator, "spec", "frontend") != frontend || value(initiator, "spec", "disableFrontend") != false ||
		text(initiator, "spec", "size") != text(volume, "spec", "size") || text(initiator, "status", "currentSize") != text(volume, "spec", "size") ||
		text(initiator, "status", "endpoint") == "" || !absentOrEmptyString(value(initiator, "status", "switchoverPhase")) ||
		text(initiator, "spec", "targetIP") != address || text(initiator, "status", "targetIP") != address ||
		value(initiator, "spec", "targetPort") != port || value(initiator, "status", "targetPort") != port {
		return fail()
	}
	copies, err := nativeRestoreAttachedReplicas(state, observed.Inventory.replicas, volume, engine, observed.Managers, used)
	if err != nil {
		return fail()
	}
	return nativeRestoreAttachmentReceipt{Consumer: benchmarkIdentity(observed.Pod), Engine: benchmarkIdentity(engine), Frontend: benchmarkIdentity(initiator),
		Replicas: copies, ConsumerNode: consumer, EngineNode: engineNode, TargetIP: address, TargetPort: int(port), Endpoint: text(initiator, "status", "endpoint"),
		ConsumerContainerID: text(process, "containerID"), ConsumerStartedAt: started.UTC().Format(time.RFC3339Nano)}, nil
}

func nativeRestoreAttachedManagerRoster(targets []nativeRestoreTarget, managers map[string]nativeRestoreV2ManagerIdentity, used map[string]bool) bool {
	if len(targets) != len(managers) {
		return false
	}
	names, containers := map[string]bool{}, map[string]bool{}
	for _, target := range targets {
		manager := managers[target.Name]
		if manager.Node != target.Name || !kubeName.MatchString(manager.Pod) || names[manager.Pod] ||
			!nativeRestoreUID(manager.ManagerUID) || !nativeRestoreUID(manager.PodUID) || manager.ManagerUID == manager.PodUID ||
			used[manager.ManagerUID] || used[manager.PodUID] || !benchmarkContainerID.MatchString(manager.ContainerID) ||
			containers[manager.ContainerID] || manager.StartedAt.IsZero() || manager.StartedAt.After(time.Now()) {
			return false
		}
		used[manager.ManagerUID], used[manager.PodUID] = true, true
		names[manager.Pod], containers[manager.ContainerID] = true, true
	}
	return true
}

func nativeRestoreInstanceOnManager(instance object, managers map[string]nativeRestoreV2ManagerIdentity) bool {
	name := managers[text(instance, "spec", "nodeID")].Pod
	return name != "" && text(instance, "status", "instanceManagerName") == name
}

func nativeRestoreReceiptUIDs(record benchmarkRecord, seed canarySeedState, state nativeRestoreState) map[string]bool {
	used := map[string]bool{}
	for _, resources := range []map[string]benchmarkResource{record.Resources, state.Resources} {
		for _, resource := range resources {
			used[resource.UID] = true
		}
	}
	for _, resource := range []benchmarkResource{state.SeedJournal, seed.Location, seed.Backup, seed.Transfer, seed.Repository} {
		used[resource.UID] = true
	}
	for _, target := range state.Targets {
		used[target.UID], used[target.Preparation.LonghornUID], used[target.Preparation.Journal.UID] = true, true, true
	}
	return used
}

func nativeRestoreInstanceIdentity(instance object, kind string, volume object, used map[string]bool) bool {
	id := benchmarkIdentity(instance)
	// Pinned Longhorn emits this single owner without controller:true. A
	// matching volume name cannot substitute for the original Volume UID.
	expectedOwner := []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": text(volume, "metadata", "name"), "uid": text(volume, "metadata", "uid")}}
	if text(instance, "apiVersion") != "longhorn.io/v1beta2" || text(instance, "kind") != kind || id.Namespace != "longhorn-system" ||
		!kubeName.MatchString(id.Name) || !nativeRestoreUID(id.UID) || text(instance, "metadata", "resourceVersion") == "" ||
		value(instance, "metadata", "deletionTimestamp") != nil || !reflect.DeepEqual(value(instance, "metadata", "ownerReferences"), expectedOwner) || used[id.UID] {
		return false
	}
	used[id.UID] = true
	return true
}

// V2's target Engine may run on another qualified node. Only its active
// EngineFrontend belongs to the consumer. Stopped old instances may remain,
// but a second live path never satisfies this stable attachment receipt.
func nativeRestoreCurrentInstance(instances []object, kind string, volume object, used map[string]bool) (object, error) {
	if _, err := indexed(instances); err != nil {
		return nil, errNativeRestoreEvidence
	}
	var current object
	for _, instance := range instances {
		if text(instance, "spec", "volumeName") != text(volume, "metadata", "name") {
			continue
		}
		if !nativeRestoreInstanceIdentity(instance, kind, volume, used) || text(instance, "spec", "dataEngine") != "v2" {
			return nil, errNativeRestoreEvidence
		}
		if value(instance, "spec", "active") != true {
			if value(instance, "spec", "active") != false || text(instance, "spec", "desireState") != "stopped" || text(instance, "status", "currentState") != "stopped" || text(instance, "status", "endpoint") != "" {
				return nil, errNativeRestoreEvidence
			}
			continue
		}
		if current != nil || text(instance, "spec", "desireState") != "running" || text(instance, "status", "currentState") != "running" {
			return nil, errNativeRestoreEvidence
		}
		current = instance
	}
	if current == nil {
		return nil, errNativeRestoreEvidence
	}
	return current, nil
}

func nativeRestoreAttachedReplicas(state nativeRestoreState, replicas []object, volume, engine object, managers map[string]nativeRestoreV2ManagerIdentity, used map[string]bool) ([3]benchmarkResource, error) {
	var result [3]benchmarkResource
	count := 0
	modes := valueObject(engine, "status", "replicaModeMap")
	for _, replica := range replicas {
		if text(replica, "spec", "volumeName") != text(volume, "metadata", "name") || value(replica, "spec", "active") != true {
			continue
		}
		if !nativeRestoreInstanceIdentity(replica, "Replica", volume, used) || !nativeRestoreInstanceOnManager(replica, managers) || text(replica, "spec", "dataEngine") != "v2" ||
			text(replica, "spec", "volumeSize") != text(volume, "spec", "size") ||
			text(replica, "spec", "engineName") != text(engine, "metadata", "name") || text(replica, "status", "currentState") != "running" || modes[text(replica, "metadata", "name")] != "RW" {
			return result, errNativeRestoreEvidence
		}
		found := false
		for i, target := range state.Targets {
			if text(replica, "spec", "nodeID") != target.Name {
				continue
			}
			mapper, err := target.Preparation.mapper(target.Name)
			if err != nil || result[i].UID != "" || text(replica, "spec", "diskID") != target.Preparation.DiskUUID || text(replica, "spec", "diskPath") != mapper {
				return result, errNativeRestoreEvidence
			}
			result[i], found = benchmarkIdentity(replica), true
		}
		if !found {
			return result, errNativeRestoreEvidence
		}
		count++
	}
	if count != 3 || len(modes) != 3 {
		return result, errNativeRestoreEvidence
	}
	return result, nil
}
