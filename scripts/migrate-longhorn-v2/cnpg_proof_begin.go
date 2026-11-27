package main

import (
	"context"
	"errors"
	"regexp"
)

// beginCNPGProof persists the native archive binding before a fresh backup write.
func beginCNPGProof(ctx context.Context, c client, source, run, target string) (*cnpgProofRun, error) {
	var selected cnpgSource
	for _, candidate := range cnpgArchiveSources() {
		if candidate.Name == target {
			selected = candidate
		}
	}
	if selected.Name == "" || !recoveryRun.MatchString(run) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(source) {
		return nil, errors.New("database proof requires fixed source and first-attempt run identities")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return nil, err
	}
	name := "longhorn-v2-db-proof-" + target
	prior, err := c.get(ctx, "configmap", "longhorn-system", name, true)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		return nil, errors.New("an existing database recovery journal requires explicit continuation")
	}
	observation, err := c.readCNPGObservation(ctx, selected)
	if err != nil {
		return nil, err
	}
	storeName := target
	if target == "wedding-db" {
		storeName = "wedding-db-dedicated"
	}
	store, err := c.get(ctx, "objectstores.barmancloud.cnpg.io", selected.Namespace, storeName, false)
	if err != nil {
		return nil, err
	}
	clusterGen, clusterGenOK := value(observation.Cluster, "metadata", "generation").(float64)
	storeGen, storeGenOK := value(store, "metadata", "generation").(float64)
	if !clusterGenOK || !storeGenOK || clusterGen < 1 || clusterGen > 2147483647 || storeGen < 1 || storeGen > 2147483647 || clusterGen != float64(int64(clusterGen)) || storeGen != float64(int64(storeGen)) {
		return nil, errors.New("native archive generations are incomplete")
	}
	archive := cnpgArchiveBinding{ClusterUID: text(observation.Cluster, "metadata", "uid"), StoreUID: text(store, "metadata", "uid"), ClusterGeneration: int64(clusterGen), StoreGeneration: int64(storeGen), Endpoint: text(store, "spec", "configuration", "endpointURL"), Destination: text(store, "spec", "configuration", "destinationPath")}
	if _, err = bindCNPGArchiveSource(observation.Cluster, store, archive); err != nil {
		return nil, err
	}
	record := cnpgProofRecord{Version: 1, Source: source, Run: run, Target: target, Phase: "planned", Archive: archive, Resources: map[string]benchmarkResource{}}
	body, err := encodeCNPGProofRecord(record)
	if err != nil {
		return nil, err
	}
	desired := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": name, "namespace": "longhorn-system", "labels": object{"storage.platform.devantler.tech/recovery-run": run, "app.kubernetes.io/managed-by": "github-actions"}}, "data": object{"record": body}}
	created, err := c.create(ctx, source, desired)
	if err != nil {
		return nil, err
	}
	readback, err := c.get(ctx, "configmap", "longhorn-system", name, false)
	if err != nil || benchmarkIdentity(readback) != benchmarkIdentity(created) || text(readback, "metadata", "namespace") != "longhorn-system" || text(readback, "data", "record") != body || text(readback, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != run || text(readback, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return nil, errors.New("database proof journal creation could not be read back")
	}
	return &cnpgProofRun{c: c, record: record, journalUID: text(created, "metadata", "uid"), storage: temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}}, nil
}
