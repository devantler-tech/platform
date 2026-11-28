package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

const providerCNPGStandbyObservation = "observe-provider-cnpg-v2-standby"

type providerCNPGStandbyGrant struct {
	Database cnpgSource
	Member   string
	Source   string
	Run      string
}

var errProviderCNPGStandby = errors.New("the exact V2 standby or its retained provider sources are unverified")

func providerCNPGStandbySource(database string) cnpgSource {
	switch database {
	case "customer-identity-db":
		return cnpgSource{Namespace: "customer-identity", Name: database}
	case "business-site-portal-db":
		return cnpgSource{Namespace: "business-site", Name: database}
	default:
		return cnpgSource{}
	}
}

func authorizeProviderCNPGStandby(getenv func(string) string) (providerCNPGStandbyGrant, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	member, prefixed := strings.CutPrefix(confirmation, providerCNPGStandbyObservation+":")
	database := providerCNPGStandbySource(getenv("LONGHORN_V2_DATABASE"))
	source, run := getenv("GITHUB_SHA"), getenv("GITHUB_RUN_ID")
	_, nameErr := cnpgInstanceID(member, database.Name)
	if getenv("LONGHORN_V2_OPERATION") != providerCNPGStandbyObservation || getenv("LONGHORN_V2_NODE") != "" ||
		database.Name == "" || !prefixed || nameErr != nil || !validBenchmarkOwner(run, source) {
		return providerCNPGStandbyGrant{}, errProviderCNPGStandby
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return providerCNPGStandbyGrant{}, err
	}
	return providerCNPGStandbyGrant{Database: database, Member: member, Source: source, Run: run}, nil
}

type providerCNPGStandbyObservationState struct {
	V2        cnpgRecoveredStorage
	Originals map[string]providerCNPGOriginal
}

type providerCNPGOriginal struct {
	Claim, PV, Attachment benchmarkResource
	Handle, Node          string
}

func observeProviderCNPGStandby(ctx context.Context, c client, grant providerCNPGStandbyGrant) error {
	if c.context != "admin@prod" || grant.Database != providerCNPGStandbySource(grant.Database.Name) || !validBenchmarkOwner(grant.Run, grant.Source) {
		return errProviderCNPGStandby
	}
	first, err := observeCNPGMigration(ctx, c, grant.Database.Name, grant.Source)
	if err != nil {
		return err
	}
	archive, err := rebindCNPGStandbyArchiveProof(ctx, c, grant.Database, first)
	if err != nil {
		return err
	}
	activation := temporaryV2Activation{c: c, source: grant.Source, owner: grant.Run, pools: c.inventoryPools}
	disks, err := activation.observeRecoveryStorage(ctx, nil)
	if err != nil || archive.Storage == nil || !recoveryStoragePreservesArchive(disks, *archive.Storage) {
		return errProviderCNPGStandby
	}
	before, err := c.readProviderCNPGStandby(ctx, grant, first, disks.Disks)
	if err != nil {
		return err
	}
	last, err := observeCNPGMigration(ctx, c, grant.Database.Name, grant.Source)
	if err != nil {
		return err
	}
	if err := validateCNPGObservation(first, last); err != nil {
		return err
	}
	after, err := c.readProviderCNPGStandby(ctx, grant, last, disks.Disks)
	if err != nil || !reflect.DeepEqual(before, after) {
		return errProviderCNPGStandby
	}
	return sourceCurrent(ctx, c.run, grant.Source)
}

func (c client) readProviderCNPGStandby(ctx context.Context, grant providerCNPGStandbyGrant, observation cnpgObservation, disks []cnpgRecoveryDiskBinding) (providerCNPGStandbyObservationState, error) {
	standby, err := readCNPGV2Standby(ctx, c, observation, grant.Member, disks)
	if err != nil {
		return providerCNPGStandbyObservationState{}, err
	}
	pods, err := c.list(ctx, "pods", grant.Database.Namespace)
	if err != nil {
		return providerCNPGStandbyObservationState{}, err
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return providerCNPGStandbyObservationState{}, err
	}
	nodes, handles := map[string]bool{}, map[string]bool{}
	originals := map[string]providerCNPGOriginal{}
	for _, pod := range observation.Pods {
		name, node := text(pod, "metadata", "name"), text(pod, "spec", "nodeName")
		if node == "" || nodes[node] {
			return providerCNPGStandbyObservationState{}, errProviderCNPGStandby
		}
		nodes[node] = true
		if name == grant.Member {
			continue
		}
		claim, err := c.get(ctx, "persistentvolumeclaim", grant.Database.Namespace, name, false)
		if err != nil || text(claim, "metadata", "name") != name || !recoveredPostgresMount(pod, claim) || !cnpgOwned(claim, observation.Cluster) ||
			!onlyCNPGStandbyUsesClaim(pods, pod, name, grant.Database.Namespace) {
			return providerCNPGStandbyObservationState{}, errProviderCNPGStandby
		}
		pv, err := c.get(ctx, "persistentvolume", "", text(claim, "spec", "volumeName"), false)
		if err != nil {
			return providerCNPGStandbyObservationState{}, err
		}
		binding, err := bindProviderCNPGOriginal(claim, pv, attachments, node)
		if err != nil {
			return providerCNPGStandbyObservationState{}, err
		}
		if handles[binding.Handle] {
			return providerCNPGStandbyObservationState{}, errProviderCNPGStandby
		}
		handles[binding.Handle] = true
		originals[name] = binding
	}
	if len(originals) != 2 {
		return providerCNPGStandbyObservationState{}, errProviderCNPGStandby
	}
	return providerCNPGStandbyObservationState{V2: standby, Originals: originals}, nil
}

func bindProviderCNPGOriginal(claim, pv object, attachments []object, node string) (providerCNPGOriginal, error) {
	pvName := text(pv, "metadata", "name")
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", text(claim, "metadata", "namespace")) ||
		!recoveredNativeObject(pv, "v1", "PersistentVolume", "") || text(claim, "spec", "storageClassName") != "hcloud" ||
		text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
		pvName != "pvc-"+text(claim, "metadata", "uid") || node == "" {
		return providerCNPGOriginal{}, errProviderCNPGStandby
	}
	selected, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
	if err != nil || len(selected) != 1 || selected[0].RequestedBytes != 10*recoveryGiB {
		return providerCNPGOriginal{}, errProviderCNPGStandby
	}
	var ticket benchmarkResource
	for _, candidate := range attachments {
		if text(candidate, "spec", "source", "persistentVolumeName") != pvName {
			continue
		}
		if ticket.UID != "" || !recoveredNativeObject(candidate, "storage.k8s.io/v1", "VolumeAttachment", "") ||
			text(candidate, "spec", "attacher") != hetznerCSIDriver || text(candidate, "spec", "nodeName") != node ||
			value(candidate, "status", "attached") != true || value(candidate, "status", "attachError") != nil ||
			value(candidate, "status", "detachError") != nil {
			return providerCNPGOriginal{}, errProviderCNPGStandby
		}
		ticket = benchmarkIdentity(candidate)
	}
	if ticket.UID == "" {
		return providerCNPGOriginal{}, errProviderCNPGStandby
	}
	return providerCNPGOriginal{Claim: benchmarkIdentity(claim), PV: benchmarkIdentity(pv), Attachment: ticket, Handle: selected[0].Handle, Node: node}, nil
}
