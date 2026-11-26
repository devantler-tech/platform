package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type cnpgArchiveProofReceipt struct {
	Backup, Cluster, Consumer, Claim, PV, Volume benchmarkResource
	EvidenceSHA256                               string
}

func (p *cnpgProofRun) recordVerifiedArchive(ctx context.Context, data cnpgRecoveredDataObservation) error {
	if p.record.Phase != "cluster-created" || p.record.Plan == nil || data.history.InRecovery ||
		data.history.SystemID != p.record.Plan.Barrier.SystemID || data.history.PostmasterStart == "" ||
		validPostgresState(data.history) != nil || data.filesystem.Device == 0 || data.filesystem.Entries == 0 ||
		data.datapath.Endpoint.PodUID != data.storage.Pod.UID || data.datapath.PolicyUID != p.record.Resources["Policy"].UID ||
		len(data.storage.Replicas) != 3 {
		return errors.New("database archive proof lacks its physical recovery, filesystem or isolation evidence")
	}
	if _, err := p.currentExecution(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Storage    cnpgRecoveredStorage
		Filesystem cnpgFilesystemBinding
		History    postgresState
		Datapath   recoveryCiliumDatapath
	}{data.storage, data.filesystem, data.history, data.datapath})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	p.record.RestoreReceipt = &cnpgArchiveProofReceipt{
		Backup: p.record.Backup, Cluster: data.storage.Cluster, Consumer: data.storage.Pod, Claim: data.storage.Claim,
		PV: data.storage.PV, Volume: data.storage.Volume, EvidenceSHA256: hex.EncodeToString(digest[:]),
	}
	p.record.Phase, p.record.RestoreVerified = "restore-verified", true
	return p.save(ctx)
}

func validateCNPGRestoreReceipt(record cnpgProofRecord) error {
	refused := errors.New("verified archive proof lacks its exact backup and recovered V2 storage identities")
	receipt := record.RestoreReceipt
	verifiedPhase := record.Phase == "restore-verified" || record.Phase == cnpgArchiveCredentialDeleteRequested || record.Phase == cnpgArchiveCredentialRetired
	if !record.RestoreVerified && receipt == nil && !verifiedPhase {
		return nil
	}
	if receipt == nil || !record.RestoreVerified || !verifiedPhase ||
		record.Plan == nil || record.Admission == nil || record.Storage == nil || record.Preparation == nil || record.Credential == nil ||
		len(record.Resources) != 5 || receipt.Backup != record.Backup || receipt.Cluster != record.Resources["Cluster"] ||
		receipt.Backup.UID != record.Plan.Source.BackupUID || receipt.Backup.Namespace != record.Plan.Source.Namespace ||
		!seedSHA256.MatchString(receipt.EvidenceSHA256) {
		return refused
	}
	namespace := record.Resources["Namespace"].Name
	backend := "pvc-" + receipt.Claim.UID
	if namespace != "longhorn-db-proof-"+record.Target+"-"+record.Run || !isolationNamespace.MatchString(namespace) ||
		receipt.Cluster.Namespace != namespace || receipt.Cluster.Name != "restore" ||
		receipt.Consumer.Namespace != namespace || receipt.Consumer.Name != "restore-1" ||
		receipt.Claim.Namespace != namespace || receipt.Claim.Name != "restore-1" ||
		receipt.PV.Namespace != "" || receipt.PV.Name != backend || receipt.Volume.Namespace != "longhorn-system" || receipt.Volume.Name != backend {
		return refused
	}
	seen := map[string]bool{}
	for _, identity := range []benchmarkResource{receipt.Backup, receipt.Cluster, receipt.Consumer, receipt.Claim, receipt.PV, receipt.Volume} {
		if !isolationUID.MatchString(identity.UID) || seen[identity.UID] {
			return refused
		}
		seen[identity.UID] = true
	}
	return nil
}
