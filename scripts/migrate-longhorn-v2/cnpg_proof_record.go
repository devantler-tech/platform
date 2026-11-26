package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
)

type cnpgProofRecord struct {
	Version                    int
	Source, Run, Target, Phase string
	Archive                    cnpgArchiveBinding
	Backup                     benchmarkResource
	Resources                  map[string]benchmarkResource
	Plan                       *cnpgRecoveryPlan
	Preparation                *recoveryPolicyPreparation
	Credential                 *cnpgCredentialBinding
	Admission                  *cnpgRecoveryAdmission
	Storage                    *cnpgStorageEnvironment
	Executor                   *cnpgProofExecutor       `json:",omitempty"`
	RestoreReceipt             *cnpgArchiveProofReceipt `json:",omitempty"`
	RestoreVerified, Cleaned   bool
}

type cnpgProofRun struct {
	c          client
	record     cnpgProofRecord
	journalUID string
	storage    temporaryV2Activation
	executing  cnpgProofExecutor
}

// save records one conditional transition without changing an established witness.
func (p *cnpgProofRun) save(ctx context.Context) error {
	owner, err := p.executionOwner()
	if err != nil {
		return err
	}
	cm, old, err := readCNPGProof(ctx, p.c, p.record.Target)
	if err != nil {
		return err
	}
	if p.journalUID == "" || text(cm, "metadata", "uid") != p.journalUID || !unchangedCNPGProofInputs(old, p.record) {
		return errors.New("database recovery journal ownership or recorded inputs changed")
	}
	allowed := old.Phase == "planned" && p.record.Phase == "backup-requested" || old.Phase == "backup-requested" && p.record.Phase == "backup-created"
	allowed = allowed || old.Phase == "backup-created" && p.record.Phase == "restore-planned" && p.record.Plan != nil
	allowed = allowed && recoveryJSONEqual(old.Resources, p.record.Resources) || validCNPGEnvironmentTransition(old, p.record)
	allowed = allowed || validCNPGCredentialTransition(old, p.record)
	allowed = allowed || validCNPGDatabaseTransition(old, p.record)
	if old.Admission == nil && p.record.Admission != nil && (old.Phase != "credential-created" || p.record.Phase != "objectstore-requested") {
		allowed = false
	}
	if old.Storage == nil && p.record.Storage != nil && (old.Phase != "credential-created" || p.record.Phase != "objectstore-requested") {
		allowed = false
	}
	if old.Credential == nil && p.record.Credential != nil && (old.Phase != "isolation-prepared" || p.record.Phase != "credential-requested") {
		allowed = false
	}
	if old.Phase == "policy-created" && p.record.Phase == "isolation-prepared" && old.Preparation == nil && p.record.Preparation != nil {
		proof := p.record.Preparation
		hostname := cnpgRecoveryHostname(old.Plan)
		allowed = recoveryJSONEqual(old.Resources, p.record.Resources) && proof.PolicyUID == old.Resources["Policy"].UID && proof.Generation > 0 && proof.Revision > 0 && hostname != "" && proof.Agent.Node == hostname && isolationUID.MatchString(proof.Agent.NodeUID) && isolationUID.MatchString(proof.Agent.BootID)
	}
	if old.Preparation == nil && p.record.Preparation != nil && (old.Phase != "policy-created" || p.record.Phase != "isolation-prepared") {
		allowed = false
	}
	if p.record.RestoreVerified || p.record.RestoreReceipt != nil {
		verified := old.Phase == "cluster-created" && p.record.Phase == "restore-verified" && !old.RestoreVerified && old.RestoreReceipt == nil
		request := old.Phase == "restore-verified" && p.record.Phase == cnpgArchiveCredentialDeleteRequested
		retired := old.Phase == cnpgArchiveCredentialDeleteRequested && p.record.Phase == cnpgArchiveCredentialRetired
		allowed = (verified || request || retired) && recoveryJSONEqual(old.Resources, p.record.Resources) && validateCNPGRestoreReceipt(p.record) == nil
	}
	if p.record.Cleaned || old.Plan == nil && p.record.Phase != "restore-planned" && p.record.Plan != nil {
		allowed = false
	}
	if !allowed {
		return errors.New("database recovery journal transition is not admitted")
	}
	body, err := encodeCNPGProofRecord(p.record)
	if err != nil {
		return err
	}
	if p.record.Executor != nil {
		if err = cnpgExecutorRunning(ctx, p.c, owner); err != nil {
			return err
		}
	}
	if err = sourceCurrent(ctx, p.c.run, owner.Source); err != nil {
		return err
	}
	if err = p.c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": body}); err != nil {
		return err
	}
	readback, _, err := readCNPGProof(ctx, p.c, p.record.Target)
	if err != nil || text(readback, "metadata", "uid") != p.journalUID || text(readback, "data", "record") != body {
		return errors.New("database recovery journal update could not be read back")
	}
	return nil
}

// freshBackup durably records intent and the native response before later stages.
func (p *cnpgProofRun) freshBackup(ctx context.Context) error {
	var source cnpgSource
	for _, candidate := range cnpgArchiveSources() {
		if candidate.Name == p.record.Target {
			source = candidate
		}
	}
	if source.Name == "" || p.record.Version != 1 || p.record.Phase != "planned" || p.record.Cleaned || p.record.RestoreVerified || !recoveryRun.MatchString(p.record.Run) || p.record.Backup.UID != "" || p.record.Archive.ClusterUID == "" || p.record.Archive.StoreUID == "" || p.record.Archive.ClusterGeneration < 1 || p.record.Archive.StoreGeneration < 1 {
		return errors.New("fresh backup requires the owned unstarted recovery record")
	}
	cluster, err := p.c.get(ctx, "cluster.postgresql.cnpg.io", source.Namespace, source.Name, false)
	if err != nil {
		return err
	}
	storeName := source.Name
	if source.Name == "wedding-db" {
		storeName = "wedding-db-dedicated"
	}
	store, err := p.c.get(ctx, "objectstores.barmancloud.cnpg.io", source.Namespace, storeName, false)
	if err != nil {
		return err
	}
	if _, err = bindCNPGArchiveSource(cluster, store, p.record.Archive); err != nil {
		return err
	}
	name := "longhorn-v2-proof-" + source.Name + "-" + p.record.Run
	p.record.Backup = benchmarkResource{Name: name, Namespace: source.Namespace}
	p.record.Phase = "backup-requested"
	if err := p.save(ctx); err != nil {
		return err
	}
	desired := object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup", "metadata": object{"name": name, "namespace": source.Namespace, "labels": object{"storage.platform.devantler.tech/recovery-run": p.record.Run, "app.kubernetes.io/managed-by": "github-actions"}}, "spec": object{"cluster": object{"name": source.Name}, "method": "plugin", "target": "primary", "pluginConfiguration": object{"name": "barman-cloud.cloudnative-pg.io"}}}
	created, err := p.createOwned(ctx, desired)
	if err != nil {
		return err
	}
	if text(created, "metadata", "namespace") != source.Namespace || text(created, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(created, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" || value(created, "metadata", "deletionTimestamp") != nil || text(created, "spec", "cluster", "name") != source.Name || text(created, "spec", "method") != "plugin" || text(created, "spec", "target") != "primary" || text(created, "spec", "pluginConfiguration", "name") != "barman-cloud.cloudnative-pg.io" || value(created, "spec", "pluginConfiguration", "parameters") != nil {
		return errors.New("created backup differs from the owned recovery request")
	}
	p.record.Backup = benchmarkIdentity(created)
	p.record.Phase = "backup-created"
	return p.save(ctx)
}

// encodeCNPGProofRecord keeps the journal payload bounded to a ConfigMap value.
func encodeCNPGProofRecord(r cnpgProofRecord) (string, error) {
	b, err := json.Marshal(r)
	if err != nil || len(b) > 65536 {
		return "", errors.New("database recovery record is incomplete or too large")
	}
	return string(b), nil
}

// unchangedCNPGProofInputs protects the original source and persisted identities.
func unchangedCNPGProofInputs(before, after cnpgProofRecord) bool {
	if before.Version != after.Version || before.Source != after.Source || before.Run != after.Run || before.Target != after.Target || before.Cleaned || !reflect.DeepEqual(before.Archive, after.Archive) || !reflect.DeepEqual(before.Executor, after.Executor) {
		return false
	}
	if before.Backup.UID != "" && before.Backup != after.Backup {
		return false
	}
	if before.Backup.Name != "" && (before.Backup.Name != after.Backup.Name || before.Backup.Namespace != after.Backup.Namespace) {
		return false
	}
	if before.Plan != nil && !recoveryJSONEqual(before.Plan, after.Plan) {
		return false
	}
	if before.Preparation != nil && !reflect.DeepEqual(before.Preparation, after.Preparation) {
		return false
	}
	if before.Credential != nil && !reflect.DeepEqual(before.Credential, after.Credential) {
		return false
	}
	if before.Admission != nil && !reflect.DeepEqual(before.Admission, after.Admission) {
		return false
	}
	if before.Storage != nil && !recoveryJSONEqual(before.Storage, after.Storage) {
		return false
	}
	if before.RestoreVerified && (!after.RestoreVerified || !reflect.DeepEqual(before.RestoreReceipt, after.RestoreReceipt)) {
		return false
	}
	for key, identity := range before.Resources {
		if after.Resources[key] != identity {
			return false
		}
	}
	return true
}
