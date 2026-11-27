package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type cnpgCredentialBinding struct {
	Source  benchmarkResource
	DataSHA string
}

// copySourceCredential keeps credential values only in memory and the owned
// immutable Secret. The journal retains only the source identity and data hash.
func (p *cnpgProofRun) copySourceCredential(ctx context.Context) (err error) {
	phase := "input"
	defer func() {
		if err != nil {
			err = fmt.Errorf("archive recovery credential copy held: phase=%s", phase)
		}
	}()
	refused := errors.New("credential copy is incomplete or changed")
	if p.c.context != "admin@prod" || p.record.Version != 1 || p.record.Phase != "isolation-prepared" || p.record.Preparation == nil || p.record.Credential != nil || p.record.Plan == nil || len(p.record.Resources) != 2 || p.record.Cleaned || p.record.RestoreVerified || !commandContextActive(ctx) {
		return refused
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	phase = "current-source"
	if _, err = p.currentExecution(ctx); err != nil {
		return err
	}
	phase = "isolation"
	if err = p.emptyRecoveryNamespace(ctx); err != nil {
		return err
	}
	input, err := p.recoveryPolicyInput(ctx)
	if err != nil {
		return err
	}
	current, err := p.c.observeRecoveryPolicyPreparation(ctx, input)
	if err != nil {
		return err
	}
	previous := *p.record.Preparation
	if current.Revision < previous.Revision {
		return refused
	}
	previous.Revision = current.Revision
	if current != previous {
		return refused
	}
	phase = "archive"
	if err = p.rebindOriginalArchive(ctx); err != nil {
		return err
	}
	source := p.record.Plan.Source
	phase = "credential-source"
	secret, err := p.c.get(ctx, "secret", source.Namespace, source.Credential, false)
	if err != nil {
		return err
	}
	desired, binding, err := recoveryCredentialCopy(secret, source, p.record.Run)
	if err != nil {
		return err
	}
	p.record.Credential, p.record.Phase = &binding, "credential-requested"
	phase = "intent"
	if err = p.save(ctx); err != nil {
		return err
	}
	phase = "source-continuity"
	latest, err := p.c.get(ctx, "secret", source.Namespace, source.Credential, false)
	if err != nil {
		return err
	}
	_, latestBinding, err := recoveryCredentialCopy(latest, source, p.record.Run)
	if err != nil || latestBinding != binding {
		return refused
	}
	phase = "namespace-continuity"
	namespace, err := p.c.get(ctx, "namespace", "", p.record.Resources["Namespace"].Name, false)
	if err != nil || benchmarkIdentity(namespace) != p.record.Resources["Namespace"] || value(namespace, "metadata", "deletionTimestamp") != nil || text(namespace, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" {
		return refused
	}
	phase = "creation"
	created, err := p.createOwned(ctx, desired)
	if err != nil {
		return err
	}
	if !isolationUID.MatchString(text(created, "metadata", "uid")) || text(created, "apiVersion") != "v1" || text(created, "kind") != "Secret" || text(created, "metadata", "name") != "archive-read" || text(created, "metadata", "namespace") != text(desired, "metadata", "namespace") || value(created, "metadata", "deletionTimestamp") != nil || text(created, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(created, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return refused
	}
	if fingerprint, e := recoveryCredentialData(created); e != nil || fingerprint != binding.DataSHA || text(created, "type") != "Opaque" || value(created, "immutable") != true {
		return refused
	}
	p.record.Resources["Secret"], p.record.Phase = benchmarkIdentity(created), "credential-created"
	phase = "identity-checkpoint"
	if err = p.save(ctx); err != nil {
		return err
	}
	phase = "readback"
	readback, err := p.c.get(ctx, "secret", text(created, "metadata", "namespace"), "archive-read", false)
	if err != nil {
		return err
	}
	if benchmarkIdentity(readback) != p.record.Resources["Secret"] || value(readback, "metadata", "deletionTimestamp") != nil || text(readback, "type") != "Opaque" || value(readback, "immutable") != true || !recoveryJSONEqual(value(readback, "metadata", "labels"), value(created, "metadata", "labels")) {
		return refused
	}
	fingerprint, err := recoveryCredentialData(readback)
	if err != nil || fingerprint != binding.DataSHA {
		return refused
	}
	return nil
}

// rebindOriginalArchive revalidates the fixed source and completed native Backup.
func (p *cnpgProofRun) rebindOriginalArchive(ctx context.Context) error {
	refused := errors.New("original recovery source changed")
	var selected cnpgSource
	for _, candidate := range cnpgArchiveSources() {
		if candidate.Name == p.record.Target {
			selected = candidate
		}
	}
	if selected.Name == "" || p.record.Plan == nil {
		return refused
	}
	cluster, err := p.c.get(ctx, "cluster.postgresql.cnpg.io", selected.Namespace, selected.Name, false)
	if err != nil {
		return err
	}
	storeName := selected.Name
	if storeName == "wedding-db" {
		storeName = "wedding-db-dedicated"
	}
	store, err := p.c.get(ctx, "objectstores.barmancloud.cnpg.io", selected.Namespace, storeName, false)
	if err != nil {
		return err
	}
	backup, err := p.c.get(ctx, "backups.postgresql.cnpg.io", selected.Namespace, p.record.Backup.Name, false)
	if err != nil {
		return err
	}
	if benchmarkIdentity(backup) != p.record.Backup || text(backup, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(backup, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" || text(backup, "spec", "target") != "primary" || value(backup, "spec", "pluginConfiguration", "parameters") != nil {
		return refused
	}
	source, err := bindCNPGRecoverySource(cluster, store, backup, p.record.Backup.UID, p.record.Archive)
	if err != nil || source != p.record.Plan.Source {
		return refused
	}
	return nil
}

// recoveryCredentialCopy selects only the three native archive references.
func recoveryCredentialCopy(secret object, source cnpgRecoverySource, run string) (object, cnpgCredentialBinding, error) {
	refused := errors.New("archive credential source is incomplete")
	if !recoveryRun.MatchString(run) || !isolationUID.MatchString(text(secret, "metadata", "uid")) || text(secret, "metadata", "resourceVersion") == "" || text(secret, "apiVersion") != "v1" || text(secret, "kind") != "Secret" || text(secret, "metadata", "name") != source.Credential || text(secret, "metadata", "namespace") != source.Namespace || value(secret, "metadata", "deletionTimestamp") != nil || text(secret, "type") != "Opaque" {
		return nil, cnpgCredentialBinding{}, refused
	}
	namespace := "longhorn-db-proof-" + source.Cluster + "-" + run
	if !isolationNamespace.MatchString(namespace) {
		return nil, cnpgCredentialBinding{}, refused
	}
	data := object{}
	for _, key := range []string{"ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "REGION"} {
		data[key] = text(secret, "data", key)
	}
	copiedSecret := object{"apiVersion": "v1", "kind": "Secret", "metadata": object{"name": "archive-read", "namespace": namespace, "labels": object{"storage.platform.devantler.tech/recovery-run": run, "app.kubernetes.io/managed-by": "github-actions"}}, "type": "Opaque", "immutable": true, "data": data}
	fingerprint, err := recoveryCredentialData(copiedSecret)
	if err != nil {
		return nil, cnpgCredentialBinding{}, err
	}
	return copiedSecret, cnpgCredentialBinding{Source: benchmarkIdentity(secret), DataSHA: fingerprint}, nil
}

func recoveryCredentialData(secret object) (string, error) {
	refused := errors.New("archive credential data is incomplete")
	data, ok := value(secret, "data").(object)
	if !ok || len(data) != 3 {
		return "", refused
	}
	for _, key := range []string{"ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "REGION"} {
		encoded, valid := data[key].(string)
		if !valid || len(encoded) == 0 || len(encoded) > 16384 {
			return "", refused
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) == 0 || base64.StdEncoding.EncodeToString(decoded) != encoded {
			return "", refused
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return "", refused
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func validCNPGCredentialTransition(before, after cnpgProofRecord) bool {
	if before.Plan == nil || after.Plan == nil || before.Preparation == nil || after.Credential == nil || len(before.Resources) != 2 {
		return false
	}
	binding := after.Credential
	if binding.Source.Name != before.Plan.Source.Credential || binding.Source.Namespace != before.Plan.Source.Namespace || !isolationUID.MatchString(binding.Source.UID) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(binding.DataSHA) {
		return false
	}
	if before.Phase == "isolation-prepared" && after.Phase == "credential-requested" && before.Credential == nil {
		return recoveryJSONEqual(before.Resources, after.Resources)
	}
	identity := after.Resources["Secret"]
	return before.Phase == "credential-requested" && after.Phase == "credential-created" && before.Credential != nil && len(after.Resources) == 3 && identity.Name == "archive-read" && identity.Namespace == before.Resources["Namespace"].Name && isolationUID.MatchString(identity.UID)
}
