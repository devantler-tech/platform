package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type cnpgRecoveryAdmission struct {
	CNPG   cnpgAdmissionEnvironment
	Barman cnpgBarmanEnvironment
}

// createDatabaseResource uses the two compiled release schemas, never caller input.
func (p *cnpgProofRun) createDatabaseResource(ctx context.Context, key string) (object, error) {
	return p.createDatabaseResourceWithSchemas(ctx, key, cnpgReviewedCRDSHA, barmanReviewedCRDSHA)
}

// createDatabaseResourceWithSchemas shares ordered admission logic; tests use synthetic schema pins.
func (p *cnpgProofRun) createDatabaseResourceWithSchemas(ctx context.Context, key, cnpgSHA, barmanSHA string) (result object, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = nil
			err = fmt.Errorf("isolated database creation held: phase=%s", phase)
		}
	}()
	index, previous, requested, completed, query := -1, "", "", "", ""
	switch key {
	case "ObjectStore":
		index, previous, requested, completed, query = 2, "credential-created", "objectstore-requested", "objectstore-created", "objectstores.barmancloud.cnpg.io"
	case "Cluster":
		index, previous, requested, completed, query = 3, "objectstore-created", "cluster-requested", "cluster-created", "cluster.postgresql.cnpg.io"
	}
	refused := errors.New("database resource lacks its current owned creation plan")
	if index < 0 || p.c.context != "admin@prod" || p.record.Version != 1 || p.record.Phase != previous || p.record.Plan == nil || len(p.record.Plan.Resources) != 4 || len(p.record.Resources) != index+1 || p.record.Preparation == nil || p.record.Credential == nil || p.record.Cleaned || p.record.RestoreVerified || !commandContextActive(ctx) {
		return nil, refused
	}
	if index == 2 && (p.record.Admission != nil || p.record.Storage != nil) || index == 3 && (p.record.Admission == nil || p.record.Storage == nil) {
		return nil, refused
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	phase = "current-source"
	if _, err = p.currentExecution(ctx); err != nil {
		return nil, err
	}
	phase = "source-and-isolation"
	if err = p.rebindOriginalArchive(ctx); err != nil {
		return nil, err
	}
	if err = p.verifyDatabaseCreationNamespace(ctx, index); err != nil {
		return nil, err
	}
	if err = p.verifyDatabaseCreationPolicy(ctx); err != nil {
		return nil, err
	}
	phase = "native-storage"
	storageBefore, err := p.qualifyDatabaseStorage(ctx)
	if err != nil {
		return nil, err
	}
	if err = p.verifyRecoveryHostCapacity(ctx, storageBefore); err != nil {
		return nil, err
	}
	phase = "operator-environment"
	before, err := p.databaseAdmissionEnvironment(ctx, cnpgSHA, barmanSHA)
	if err != nil {
		return nil, err
	}
	if p.record.Admission != nil && !recoveryJSONEqual(*p.record.Admission, before) {
		return nil, refused
	}
	desired := p.record.Plan.Resources[index]
	if text(desired, "kind") != key || text(desired, "metadata", "namespace") != p.record.Resources["Namespace"].Name {
		return nil, refused
	}
	body, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	phase = "server-admission"
	raw, err := p.c.kubectl(ctx, body, "create", "--filename=/dev/stdin", "--dry-run=server", "--output=json")
	if err != nil {
		return nil, err
	}
	var admitted object
	if len(raw) > 65536 || json.Unmarshal(raw, &admitted) != nil {
		return nil, refused
	}
	compare := func(actual object) error {
		if key == "ObjectStore" {
			_, e := matchRecoveryObjectStoreAdmission(desired, actual)
			return e
		}
		_, e := matchRecoveryAdmission(desired, actual, before.CNPG.Defaults)
		return e
	}
	if err = compare(admitted); err != nil {
		return nil, err
	}
	phase = "admission-continuity"
	after, err := p.databaseAdmissionEnvironment(ctx, cnpgSHA, barmanSHA)
	if err != nil || !recoveryJSONEqual(before, after) {
		return nil, refused
	}
	if err = p.rebindOriginalArchive(ctx); err != nil {
		return nil, err
	}
	if err = p.verifyDatabaseCreationNamespace(ctx, index); err != nil {
		return nil, err
	}
	if err = p.verifyDatabaseCreationPolicy(ctx); err != nil {
		return nil, err
	}
	phase = "storage-continuity"
	storageAfter, err := p.qualifyDatabaseStorage(ctx)
	if err != nil || !recoveryJSONEqual(storageBefore, storageAfter) {
		return nil, refused
	}
	phase = "intent"
	if p.record.Admission == nil {
		p.record.Admission = &before
	}
	if p.record.Storage == nil {
		p.record.Storage = &storageBefore
	}
	p.record.Phase = requested
	if err = p.save(ctx); err != nil {
		return nil, err
	}
	phase = "storage-before-creation"
	storageCurrent, err := p.qualifyDatabaseStorage(ctx)
	if err != nil {
		return nil, err
	}
	phase = "namespace-continuity"
	if err = p.verifyDatabaseCreationNamespace(ctx, index); err != nil {
		return nil, err
	}
	if err = p.verifyRecoveryHostCapacity(ctx, storageCurrent); err != nil {
		return nil, err
	}
	phase = "creation"
	created, err := p.createOwned(ctx, desired)
	if err != nil {
		return nil, err
	}
	if !isolationUID.MatchString(text(created, "metadata", "uid")) || text(created, "apiVersion") != text(desired, "apiVersion") || text(created, "kind") != key || text(created, "metadata", "name") != text(desired, "metadata", "name") || text(created, "metadata", "namespace") != p.record.Resources["Namespace"].Name {
		return nil, refused
	}
	p.record.Resources[key], p.record.Phase = benchmarkIdentity(created), completed
	phase = "identity-checkpoint"
	if err = p.save(ctx); err != nil {
		return nil, err
	}
	phase = "created-specification"
	if err = compare(created); err != nil {
		return nil, err
	}
	phase = "readback"
	readback, err := p.c.get(ctx, query, text(desired, "metadata", "namespace"), text(desired, "metadata", "name"), false)
	if err != nil || benchmarkIdentity(readback) != p.record.Resources[key] {
		return nil, refused
	}
	if err = compare(readback); err != nil {
		return nil, err
	}
	phase = "process-continuity"
	final, err := p.databaseAdmissionEnvironment(ctx, cnpgSHA, barmanSHA)
	if err != nil || !recoveryJSONEqual(before, final) {
		return nil, refused
	}
	return readback, nil
}

func (p *cnpgProofRun) databaseAdmissionEnvironment(ctx context.Context, cnpgSHA, barmanSHA string) (cnpgRecoveryAdmission, error) {
	cnpg, err := collectCNPGAdmissionEnvironment(ctx, p.c, cnpgSHA)
	if err != nil {
		return cnpgRecoveryAdmission{}, err
	}
	barman, err := collectBarmanEnvironment(ctx, p.c, barmanSHA)
	if err != nil {
		return cnpgRecoveryAdmission{}, err
	}
	return cnpgRecoveryAdmission{CNPG: cnpg, Barman: barman}, nil
}

// verifyDatabaseCreationPolicy requires the same imported deny boundary before each data write.
func (p *cnpgProofRun) verifyDatabaseCreationPolicy(ctx context.Context) error {
	input, err := p.recoveryPolicyInput(ctx)
	if err != nil {
		return err
	}
	current, err := p.c.observeRecoveryPolicyPreparation(ctx, input)
	if err != nil {
		return err
	}
	prior := *p.record.Preparation
	if current.Revision < prior.Revision {
		return errors.New("recovery policy revision regressed")
	}
	prior.Revision = current.Revision
	if current != prior {
		return errors.New("recovery policy process changed")
	}
	return nil
}

// verifyDatabaseCreationNamespace admits only the owned immutable credential and, at stage 3, its recorded store.
func (p *cnpgProofRun) verifyDatabaseCreationNamespace(ctx context.Context, index int) error {
	refused := errors.New("database recovery namespace is replaced or populated")
	identity := p.record.Resources["Namespace"]
	readNamespace := func() error {
		ns, e := p.c.get(ctx, "namespace", "", identity.Name, false)
		if e != nil || identity.Name != "longhorn-db-proof-"+p.record.Target+"-"+p.record.Run || benchmarkIdentity(ns) != identity || value(ns, "metadata", "deletionTimestamp") != nil || text(ns, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" || text(ns, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(ns, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
			return refused
		}
		return nil
	}
	if err := readNamespace(); err != nil {
		return err
	}
	for _, kind := range []string{"pods", "jobs", "deployments", "statefulsets", "daemonsets", "cronjobs", "replicasets", "persistentvolumeclaims", "secrets", "clusters.postgresql.cnpg.io", "objectstores.barmancloud.cnpg.io"} {
		items, err := p.c.list(ctx, kind, identity.Name)
		if err != nil {
			return refused
		}
		expected := 0
		if kind == "secrets" || kind == "objectstores.barmancloud.cnpg.io" && index == 3 {
			expected = 1
		}
		if len(items) != expected {
			return refused
		}
		if kind == "secrets" {
			secret := items[0]
			fingerprint, e := recoveryCredentialData(secret)
			if e != nil || fingerprint != p.record.Credential.DataSHA || benchmarkIdentity(secret) != p.record.Resources["Secret"] || text(secret, "type") != "Opaque" || value(secret, "immutable") != true || value(secret, "metadata", "deletionTimestamp") != nil || text(secret, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != p.record.Run || text(secret, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
				return refused
			}
		}
		if kind == "objectstores.barmancloud.cnpg.io" && expected == 1 {
			if benchmarkIdentity(items[0]) != p.record.Resources["ObjectStore"] {
				return refused
			}
			if _, e := matchRecoveryObjectStoreAdmission(p.record.Plan.Resources[2], items[0]); e != nil {
				return refused
			}
		}
	}
	source := p.record.Plan.Source
	secret, err := p.c.get(ctx, "secret", source.Namespace, source.Credential, false)
	if err != nil {
		return refused
	}
	_, binding, err := recoveryCredentialCopy(secret, source, p.record.Run)
	if err != nil || binding != *p.record.Credential {
		return refused
	}
	return readNamespace()
}

func validCNPGDatabaseTransition(before, after cnpgProofRecord) bool {
	if before.Plan == nil || after.Plan == nil || len(before.Plan.Resources) != 4 || len(after.Plan.Resources) != 4 || before.Preparation == nil || before.Credential == nil || after.Admission == nil || after.Storage == nil {
		return false
	}
	for i, step := range []struct{ key, previous, requested, completed string }{
		{"ObjectStore", "credential-created", "objectstore-requested", "objectstore-created"},
		{"Cluster", "objectstore-created", "cluster-requested", "cluster-created"},
	} {
		if before.Phase == step.previous && after.Phase == step.requested {
			return len(before.Resources) == i+3 && recoveryJSONEqual(before.Resources, after.Resources) && (i == 0 && before.Admission == nil && before.Storage == nil || i == 1 && before.Admission != nil && before.Storage != nil)
		}
		if before.Phase == step.requested && after.Phase == step.completed && before.Admission != nil && before.Storage != nil {
			identity, desired := after.Resources[step.key], before.Plan.Resources[i+2]
			return len(before.Resources) == i+3 && len(after.Resources) == i+4 && identity.Name == text(desired, "metadata", "name") && identity.Namespace == text(desired, "metadata", "namespace") && isolationUID.MatchString(identity.UID)
		}
	}
	return false
}
