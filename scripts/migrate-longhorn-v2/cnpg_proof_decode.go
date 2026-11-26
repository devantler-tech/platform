package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// decodeCNPGProofRecord refuses ambiguous evidence before a journal transition.
func decodeCNPGProofRecord(raw []byte) (cnpgProofRecord, error) {
	refused := errors.New("database recovery journal is malformed or ambiguous")
	trimmed := bytes.TrimSpace(raw)
	fields := cnpgProofJSONFields()
	if len(trimmed) == 0 || len(raw) > 65536 || trimmed[0] != '{' || strictjson.Validate(raw, fields) != nil {
		return cnpgProofRecord{}, refused
	}
	var record cnpgProofRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || validateCNPGRestoreReceipt(record) != nil {
		return cnpgProofRecord{}, refused
	}
	for key := range record.Resources {
		if !slices.Contains(fields["Resources"], key) {
			return cnpgProofRecord{}, refused
		}
	}
	if record.Storage != nil {
		for key := range record.Storage.Targets {
			if !slices.Contains(fields["Storage.Targets"], key) {
				return cnpgProofRecord{}, refused
			}
		}
	}
	return record, nil
}

// cnpgProofJSONFields binds typed witness names without constraining arbitrary
// Kubernetes specifications inside Plan.Resources. Duplicate keys remain refused
// recursively, including inside those specifications.
func cnpgProofJSONFields() map[string][]string {
	identity := []string{"Name", "Namespace", "UID"}
	defaults := []string{"Version", "ConfigMapQueries", "SecretQueries"}
	crd := []string{"UID", "SpecSHA", "Generation"}
	operator := []string{"Pod", "PodUID", "ReplicaSetUID", "DeploymentUID", "Node", "NodeUID", "BootID", "Architecture", "Image", "Container", "StartedAt", "SpecSHA", "ControllerSHA", "Restarts", "ReplicaSetGeneration"}
	activation := []string{"selection", "name", "nodeUID", "providerID", "machineUUID", "bootID", "osImage", "longhornUID", "ephemeralUUID", "rawPartitionUUID", "preparationUID"}
	fields := map[string][]string{
		"":                                  {"Version", "Source", "Run", "Target", "Phase", "Archive", "Backup", "Resources", "Plan", "Preparation", "Credential", "Admission", "Storage", "RestoreVerified", "Cleaned"},
		"Archive":                           {"ClusterUID", "StoreUID", "Endpoint", "Destination", "ClusterGeneration", "StoreGeneration"},
		"Backup":                            identity,
		"Resources":                         {"Namespace", "Policy", "Secret", "ObjectStore", "Cluster"},
		"Plan":                              {"Source", "Barrier", "Resources"},
		"Plan.Source":                       {"Server", "Database", "BackupUID", "BackupID", "Namespace", "Cluster", "Store", "Credential", "Destination", "Endpoint", "Image", "Size", "ClusterUID", "StoreUID"},
		"Plan.Barrier":                      {"system_id", "timeline", "flush_lsn", "replay_lsn", "in_recovery", "postmaster_start"},
		"Preparation":                       {"Agent", "Runtime", "PolicyUID", "Generation", "Revision"},
		"Preparation.Agent":                 {"Pod", "PodUID", "Node", "NodeUID", "BootID", "DaemonSetUID", "DeclaredImage", "ImageID", "ContainerID", "StartedAt", "DaemonSetGeneration", "Restarts"},
		"Preparation.Runtime":               {"Version", "Cluster", "Enforcement", "DefaultLocalCluster", "NonDefaultDeny", "L7", "RestoreState"},
		"Credential":                        {"Source", "DataSHA"},
		"Credential.Source":                 identity,
		"Admission":                         {"CNPG", "Barman"},
		"Admission.CNPG":                    {"CRD", "DeploymentUID", "DeploymentSHA", "DeploymentGeneration", "ConfigurationUID", "ConfigurationVersion", "Operators", "Defaults", "Startup"},
		"Admission.CNPG.CRD":                crd,
		"Admission.CNPG.Operators[]":        operator,
		"Admission.CNPG.Defaults":           defaults,
		"Admission.CNPG.Startup[]":          {"Defaults", "StartedAt", "LoadedAt", "ConfigSHA", "OperatorPullSecretName", "CreateAnyService"},
		"Admission.CNPG.Startup[].Defaults": defaults,
		"Admission.Barman":                  {"CRD", "DeploymentGeneration", "Operator"},
		"Admission.Barman.CRD":              crd,
		"Admission.Barman.Operator":         operator,
		"Storage":                           {"Activation", "Targets", "Disks"},
		"Storage.Activation":                identity,
		"Storage.Targets":                   {"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"},
		"Storage.Disks[]":                   {"Selection", "Node", "NodeUID", "DiskUUID", "InstanceManager"},
	}
	for _, kind := range fields["Resources"] {
		fields["Resources."+kind] = identity
	}
	for _, selection := range fields["Storage.Targets"] {
		fields["Storage.Targets."+selection] = activation
	}
	fields[""] = append(fields[""], "Executor")
	fields["Executor"] = []string{"Source", "Run", "JournalUID"}
	fields[""] = append(fields[""], "RestoreReceipt")
	fields["RestoreReceipt"] = []string{"Backup", "Cluster", "Consumer", "Claim", "PV", "Volume", "EvidenceSHA256"}
	for _, kind := range fields["RestoreReceipt"][:6] {
		fields["RestoreReceipt."+kind] = identity
	}
	return fields
}
