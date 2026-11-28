package main

import (
	"context"
	"fmt"
	"time"
)

const (
	observeCorootClickHouseOperation = "observe-coroot-v2-clickhouse"
	corootClickHouseNamespace        = "observability"
	corootClickHouseController       = "coroot-clickhouse-shard-0"
	corootClickHouseContainer        = "clickhouse-server"
	corootClickHouseImage            = "ghcr.io/coroot/clickhouse@sha256:7a7bced745cb7975ce8b249da1e08192faf8adbcc24aae45060ccc6d7c9187a3"
)

const corootClickHouseTablesQuery = `
SELECT database AS Database, name AS Name, engine AS Engine
FROM system.tables
WHERE database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')
  AND is_temporary = 0
ORDER BY database, name
LIMIT 2049
FORMAT JSONEachRow`

const corootClickHouseReplicasQuery = `
SELECT database AS Database, table AS Name, engine AS Engine,
       replica_name AS Replica, zookeeper_path AS KeeperPath,
       toString(is_readonly) AS ReadOnly, toString(is_session_expired) AS SessionExpired,
       toString(parts_to_check) AS PartsToCheck, toString(absolute_delay) AS DelaySeconds,
       active_replicas AS ActiveReplicas, total_replicas AS TotalReplicas,
       empty(last_queue_update_exception) AS QueueErrorAbsent,
       empty(zookeeper_exception) AS KeeperErrorAbsent
FROM system.replicas
WHERE database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')
ORDER BY database, table
LIMIT 2049
FORMAT JSONEachRow`

type corootClickHouseSource struct {
	Pod    benchmarkResource
	Node   string
	Volume providerVolumeClaim
}

type corootClickHouseSourceSet struct {
	Controller benchmarkResource
	Generation float64
	Revision   string
	Members    [2]corootClickHouseSource
}

func authorizeCorootClickHouseObservation(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != observeCorootClickHouseOperation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return errCorootClickHouseStorage
	}
	return authorizeWorkflow(getenv, observeCorootClickHouseOperation)
}

func observeCorootClickHouse(ctx context.Context, c client, source string) (corootClickHouseStorageSummary, error) {
	var none corootClickHouseStorageSummary
	if c.context != "admin@prod" || !commandContextActive(ctx) {
		return none, errCorootClickHouseStorage
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return none, errCorootClickHouseStorage
	}
	before, err := readCorootClickHouseSources(ctx, c)
	if err != nil {
		return none, err
	}
	var names [2]string
	var tables [2][]corootClickHouseTable
	var replicas [2][]corootClickHouseReplica
	for i, member := range before.Members {
		names[i] = member.Pod.Name
		rawTables, err := c.readCorootClickHouseCatalog(ctx, member, corootClickHouseTablesQuery)
		if err != nil {
			return none, err
		}
		tables[i], err = readClickHouseStorageRows[corootClickHouseTable](rawTables)
		if err != nil {
			return none, err
		}
		rawReplicas, err := c.readCorootClickHouseCatalog(ctx, member, corootClickHouseReplicasQuery)
		if err != nil {
			return none, err
		}
		replicas[i], err = readClickHouseStorageRows[corootClickHouseReplica](rawReplicas)
		if err != nil {
			return none, err
		}
	}
	after, err := readCorootClickHouseSources(ctx, c)
	if err != nil || before != after || sourceCurrent(ctx, c.run, source) != nil || !commandContextActive(ctx) {
		return none, errCorootClickHouseStorage
	}
	return classifyCorootClickHouseStorage(names, tables, replicas)
}

func (c client) readCorootClickHouseCatalog(ctx context.Context, member corootClickHouseSource, query string) ([]byte, error) {
	if c.context != "admin@prod" || member.Pod.Namespace != corootClickHouseNamespace || !isolationUID.MatchString(member.Pod.UID) ||
		(member.Pod.Name != corootClickHouseController+"-0" && member.Pod.Name != corootClickHouseController+"-1") ||
		(query != corootClickHouseTablesQuery && query != corootClickHouseReplicasQuery) {
		return nil, errCorootClickHouseStorage
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// The pinned client uses the container's existing password environment.
	// Its direct loopback connection uses ClickHouse's read-only query mode.
	raw, err := c.kubectl(ctx, nil, "exec", member.Pod.Name, "--namespace="+corootClickHouseNamespace,
		"--container="+corootClickHouseContainer, "--", "/usr/bin/clickhouse", "client",
		"--host=127.0.0.1", "--port=9000", "--user=default", "--readonly=1", "--query", query)
	if err != nil {
		return nil, errCorootClickHouseStorage
	}
	return raw, nil
}

func readCorootClickHouseSources(ctx context.Context, c client) (corootClickHouseSourceSet, error) {
	var none, current corootClickHouseSourceSet
	sts, err := c.get(ctx, "statefulset", corootClickHouseNamespace, corootClickHouseController, false)
	if err != nil || !recoveredNativeObject(sts, "apps/v1", "StatefulSet", corootClickHouseNamespace) ||
		text(sts, "metadata", "name") != corootClickHouseController || value(sts, "spec", "replicas") != float64(2) ||
		value(sts, "status", "readyReplicas") != float64(2) || value(sts, "status", "updatedReplicas") != float64(2) {
		return none, errCorootClickHouseStorage
	}
	generation, ok := value(sts, "metadata", "generation").(float64)
	revision := text(sts, "status", "currentRevision")
	if !ok || generation < 1 || value(sts, "status", "observedGeneration") != generation ||
		revision == "" || text(sts, "status", "updateRevision") != revision {
		return none, errCorootClickHouseStorage
	}
	current.Controller, current.Generation, current.Revision = benchmarkIdentity(sts), generation, revision
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return none, errCorootClickHouseStorage
	}
	for ordinal := range current.Members {
		member, err := readCorootClickHouseSource(ctx, c, current.Controller, revision, ordinal, attachments)
		if err != nil {
			return none, err
		}
		current.Members[ordinal] = member
	}
	if current.Members[0].Node == current.Members[1].Node ||
		current.Members[0].Volume.Handle == current.Members[1].Volume.Handle {
		return none, errCorootClickHouseStorage
	}
	return current, nil
}

func readCorootClickHouseSource(ctx context.Context, c client, controller benchmarkResource, revision string,
	ordinal int, attachments []object) (corootClickHouseSource, error) {
	var none corootClickHouseSource
	name := fmt.Sprintf("%s-%d", corootClickHouseController, ordinal)
	claimName := "data-" + name
	pod, err := c.get(ctx, "pod", corootClickHouseNamespace, name, false)
	if err != nil || !corootClickHousePodUsesSource(pod, controller, revision, name, claimName) {
		return none, errCorootClickHouseStorage
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", corootClickHouseNamespace, claimName, false)
	if err != nil || !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", corootClickHouseNamespace) ||
		text(claim, "metadata", "name") != claimName {
		return none, errCorootClickHouseStorage
	}
	pv, err := c.get(ctx, "persistentvolume", "", text(claim, "spec", "volumeName"), false)
	if err != nil || !recoveredNativeObject(pv, "v1", "PersistentVolume", "") {
		return none, errCorootClickHouseStorage
	}
	volumes, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
	if err != nil || len(volumes) != 1 || volumes[0].ClaimUID != text(claim, "metadata", "uid") {
		return none, errCorootClickHouseStorage
	}
	node := text(pod, "spec", "nodeName")
	count := 0
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") != volumes[0].PV {
			continue
		}
		if !recoveredNativeObject(attachment, "storage.k8s.io/v1", "VolumeAttachment", "") ||
			text(attachment, "spec", "attacher") != hetznerCSIDriver || text(attachment, "spec", "nodeName") != node ||
			value(attachment, "status", "attached") != true {
			return none, errCorootClickHouseStorage
		}
		count++
	}
	if count != 1 {
		return none, errCorootClickHouseStorage
	}
	return corootClickHouseSource{Pod: benchmarkIdentity(pod), Node: node, Volume: volumes[0]}, nil
}

func corootClickHousePodUsesSource(pod object, controller benchmarkResource, revision, name, claim string) bool {
	if !recoveredNativeObject(pod, "v1", "Pod", corootClickHouseNamespace) || text(pod, "metadata", "name") != name ||
		text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) ||
		!kubeNamespace.MatchString(text(pod, "spec", "nodeName")) ||
		text(pod, "metadata", "labels", "controller-revision-hash") != revision {
		return false
	}
	refs, _ := value(pod, "metadata", "ownerReferences").([]any)
	if len(refs) != 1 {
		return false
	}
	ref, _ := refs[0].(object)
	if text(ref, "apiVersion") != "apps/v1" || text(ref, "kind") != "StatefulSet" ||
		text(ref, "name") != controller.Name || text(ref, "uid") != controller.UID || value(ref, "controller") != true {
		return false
	}
	volumes, _ := value(pod, "spec", "volumes").([]any)
	matches := 0
	for _, raw := range volumes {
		volume, _ := raw.(object)
		if value(volume, "persistentVolumeClaim") == nil {
			continue
		}
		if text(volume, "name") != "data" || text(volume, "persistentVolumeClaim", "claimName") != claim ||
			value(volume, "persistentVolumeClaim", "readOnly") == true {
			return false
		}
		matches++
	}
	if matches != 1 {
		return false
	}
	// The pinned container resolves the loopback client and its prepared
	// credentials. Bind the data mount to that container as well.
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if len(statuses) != 1 {
		return false
	}
	status, _ := statuses[0].(object)
	if text(status, "name") != corootClickHouseContainer || value(status, "ready") != true ||
		text(status, "imageID") != corootClickHouseImage {
		return false
	}
	containers, _ := value(pod, "spec", "containers").([]any)
	if len(containers) != 1 {
		return false
	}
	container, _ := containers[0].(object)
	if text(container, "name") != corootClickHouseContainer {
		return false
	}
	mounts, _ := value(container, "volumeMounts").([]any)
	for _, raw := range mounts {
		mount, _ := raw.(object)
		if text(mount, "name") == "data" && text(mount, "mountPath") == "/var/lib/clickhouse" &&
			text(mount, "subPath") == "" && text(mount, "subPathExpr") == "" && value(mount, "readOnly") != true {
			return true
		}
	}
	return false
}
