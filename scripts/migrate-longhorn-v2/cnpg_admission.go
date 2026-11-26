package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

type recoveryAdmissionDefaults struct {
	Version                         string
	ConfigMapQueries, SecretQueries bool
}

func recoveryJSONEqual(a, b any) bool {
	left, e1 := json.Marshal(a)
	right, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && len(left) <= 65536 && len(right) <= 65536 && bytes.Equal(left, right)
}

func cloneRecoverySpec(v any) (object, error) {
	raw, err := json.Marshal(v)
	var result object
	if err != nil || len(raw) > 65536 || decodeIsolationJSON(raw, &result) != nil || result == nil {
		return nil, errors.New("recovery specification could not be bound")
	}
	return result, nil
}

func addRecoveryDefaults(spec, defaults object) {
	for key, expected := range defaults {
		current, exists := spec[key]
		if !exists {
			spec[key] = expected
			continue
		}
		child, childOK := current.(object)
		addition, additionOK := expected.(object)
		if childOK && additionOK {
			addRecoveryDefaults(child, addition)
		}
	}
}

// Compare the exact owned renderer proposal with native CNPG 1.30.2 admission,
// without stripping unknown fields. This is not source/ownership/readiness or
// creation authority. The caller must independently bind the installed CRD,
// operator image/process and its startup-loaded configuration; these flags may
// never be inferred from the admitted object. See cnpg-admission-profile.md.
func matchRecoveryAdmission(proposed, admitted object, profile recoveryAdmissionDefaults) (object, error) {
	refused := errors.New("server admission differed from the pinned database recovery projection")
	ns := text(proposed, "metadata", "namespace")
	run := text(proposed, "metadata", "labels", "storage.platform.devantler.tech/recovery-run")
	if profile.Version != "1.30.2" || !isolationNamespace.MatchString(ns) || !recoveryRun.MatchString(run) || !strings.HasSuffix(ns, "-"+run) || text(proposed, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return nil, refused
	}
	for _, o := range []object{proposed, admitted} {
		if text(o, "apiVersion") != "postgresql.cnpg.io/v1" || text(o, "kind") != "Cluster" || text(o, "metadata", "name") != "restore" || text(o, "metadata", "namespace") != ns || value(o, "metadata", "deletionTimestamp") != nil {
			return nil, refused
		}
		for _, field := range []string{"annotations", "ownerReferences", "finalizers"} {
			if value(o, "metadata", field) != nil {
				return nil, refused
			}
		}
	}
	if !recoveryJSONEqual(value(proposed, "metadata", "labels"), value(admitted, "metadata", "labels")) {
		return nil, refused
	}
	expected, err := cloneRecoverySpec(value(proposed, "spec"))
	if err != nil {
		return nil, refused
	}
	required := map[string]bool{"instances": true, "imageName": true, "enableSuperuserAccess": true, "enablePDB": true, "plugins": true, "storage": true, "resources": true, "bootstrap": true, "externalClusters": true, "affinity": true}
	for key := range required {
		if _, present := expected[key]; !present {
			return nil, refused
		}
	}
	for key := range expected {
		if !required[key] {
			return nil, refused
		}
	}
	tag, digest, pinned := strings.Cut(text(expected, "imageName"), "@")
	if expected["instances"] != float64(1) || expected["enableSuperuserAccess"] != false || expected["enablePDB"] != false || !pinned || !recoveryImage.MatchString(tag) || !recoveryDigest.MatchString("ghcr.io/cloudnative-pg/postgresql@"+digest) || text(expected, "storage", "storageClass") != "longhorn-v2" || !recoverySize.MatchString(text(expected, "storage", "size")) ||
		!recoveryJSONEqual(expected["plugins"], []any{object{"name": "barman-cloud.cloudnative-pg.io", "enabled": false, "isWALArchiver": false}}) {
		return nil, refused
	}
	storage, storageOK := expected["storage"].(object)
	bootstrap, bootstrapOK := expected["bootstrap"].(object)
	recovery, recoveryOK := value(bootstrap, "recovery").(object)
	target, targetOK := value(recovery, "recoveryTarget").(object)
	if !storageOK || len(storage) != 2 || !bootstrapOK || len(bootstrap) != 1 || !recoveryOK || len(recovery) != 2 || text(recovery, "source") != "archive" || !targetOK || len(target) != 3 || !recoveryBackupID.MatchString(text(target, "backupID")) || text(target, "targetTLI") == "" || text(target, "targetLSN") == "" ||
		!recoveryJSONEqual(expected["resources"], object{"requests": object{"cpu": "50m", "memory": "256Mi"}, "limits": object{"cpu": "1", "memory": "1Gi"}}) {
		return nil, refused
	}
	if affinity, present := expected["affinity"]; present {
		a, ok := affinity.(object)
		selector, selectorOK := value(a, "nodeSelector").(object)
		hostname := text(selector, "kubernetes.io/hostname")
		if !ok || len(a) != 1 || !selectorOK || len(selector) != 1 || len(hostname) > 63 || !kubeName.MatchString(hostname) {
			return nil, refused
		}
	}
	external, valid := expected["externalClusters"].([]any)
	if !valid || len(external) != 1 {
		return nil, refused
	}
	entry, valid := external[0].(object)
	plugin, pluginOK := value(entry, "plugin").(object)
	if !valid || len(entry) != 2 || text(entry, "name") != "archive" || !pluginOK || len(plugin) != 3 || text(plugin, "name") != "barman-cloud.cloudnative-pg.io" || plugin["enabled"] != true {
		return nil, refused
	}
	parameters, parametersOK := value(plugin, "parameters").(object)
	if !parametersOK || len(parameters) != 2 || text(parameters, "barmanObjectName") != "restore-source" || len(text(parameters, "serverName")) > 253 || !kubeName.MatchString(text(parameters, "serverName")) {
		return nil, refused
	}
	plugin["isWALArchiver"] = false
	addRecoveryDefaults(expected, object{
		"affinity": object{"podAntiAffinityType": "preferred"}, "failoverDelay": 0, "logLevel": "info", "maxSyncReplicas": 0, "minSyncReplicas": 0,
		"monitoring": object{"disableDefaultQueries": false, "enablePodMonitor": false}, "postgresGID": 26, "postgresUID": 26,
		"primaryUpdateMethod": "restart", "primaryUpdateStrategy": "unsupervised",
		"probes":               object{"liveness": object{"isolationCheck": object{"enabled": true, "requestTimeout": 1000, "connectionTimeout": 1000}}},
		"replicationSlots":     object{"highAvailability": object{"enabled": true, "slotPrefix": "_cnpg_"}, "synchronizeReplicas": object{"enabled": true}, "updateInterval": 30},
		"smartShutdownTimeout": 180, "startDelay": 3600, "stopDelay": 1800, "switchoverDelay": 3600,
		"bootstrap": object{"recovery": object{"database": "app", "owner": "app"}}, "storage": object{"resizeInUseVolumes": true},
		"postgresql": object{"syncReplicaElectionConstraint": object{"enabled": false}, "parameters": object{
			"archive_mode": "on", "archive_timeout": "5min", "dynamic_shared_memory_type": "posix", "full_page_writes": "on",
			"log_destination": "csvlog", "log_directory": "/controller/log", "log_filename": "postgres", "log_rotation_age": "0", "log_rotation_size": "0", "log_truncate_on_rotation": "false", "logging_collector": "on",
			"max_parallel_workers": "32", "max_replication_slots": "32", "max_worker_processes": "32", "shared_memory_type": "mmap", "shared_preload_libraries": "",
			"ssl_max_protocol_version": "TLSv1.3", "ssl_min_protocol_version": "TLSv1.3", "wal_keep_size": "512MB", "wal_level": "logical", "wal_log_hints": "on", "wal_receiver_timeout": "5s", "wal_sender_timeout": "5s",
		}},
	})
	monitoring := expected["monitoring"].(object)
	for key, enabled := range map[string]bool{"customQueriesConfigMap": profile.ConfigMapQueries, "customQueriesSecret": profile.SecretQueries} {
		if enabled {
			monitoring[key] = []any{object{"name": "cnpg-default-monitoring", "key": "queries"}}
		}
	}
	if !recoveryJSONEqual(expected, value(admitted, "spec")) {
		return nil, refused
	}
	return cloneRecoverySpec(expected)
}
