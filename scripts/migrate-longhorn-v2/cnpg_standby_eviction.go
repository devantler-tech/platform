package main

import (
	"context"
	"encoding/json"
	"errors"
)

// Ask the Kubernetes Eviction API to exercise admission and its PDB checks for
// the exact observed standby with persistence disabled at both API boundaries.
func verifyUmamiStandbyEvictionAdmission(ctx context.Context, c client, source string, witness cnpgStandbyCutover) error {
	const namespace = "umami"
	pod := witness.Consumer
	refused := errors.New("the exact standby did not pass the Kubernetes dry-run eviction")
	if _, err := cnpgInstanceID(pod.Name, "umami-db"); err != nil || pod.Namespace != namespace ||
		!nativeRestoreUID(pod.UID) || witness.ConsumerVersion == "" || witness.BudgetVersion == "" ||
		c.context != "admin@prod" || c.run == nil {
		return refused
	}
	input := object{"apiVersion": "policy/v1", "kind": "Eviction",
		"metadata": object{"name": pod.Name, "namespace": namespace},
		"deleteOptions": object{"dryRun": []string{"All"}, "preconditions": object{
			"uid": pod.UID, "resourceVersion": witness.ConsumerVersion,
		}},
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return refused
	}
	if err := recheckUmamiStandbyAdmission(ctx, c, witness); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + namespace + "/pods/" + pod.Name + "/eviction?dryRun=All"
	raw, err := c.kubectl(ctx, encoded, "create", "--raw", path, "-f", "-")
	var status object
	if err != nil || json.Unmarshal(raw, &status) != nil || text(status, "apiVersion") != "v1" ||
		text(status, "kind") != "Status" || text(status, "status") != "Success" {
		return refused
	}
	if err := recheckUmamiStandbyAdmission(ctx, c, witness); err != nil {
		return err
	}
	return sourceCurrent(ctx, c.run, source)
}

// A dry-run success can also mean that no budget matched. Check the same
// budget revision, pod, retained volume and sole consumer on both sides.
func recheckUmamiStandbyAdmission(ctx context.Context, c client, witness cnpgStandbyCutover) error {
	refused := errors.New("the standby, disruption budget or retained storage changed during admission")
	var cluster, claim, pv, volume, budget object
	for _, resource := range []struct {
		kind     string
		identity benchmarkResource
		dest     *object
	}{
		{"clusters.postgresql.cnpg.io", witness.Database, &cluster},
		{"persistentvolumeclaim", witness.Claim, &claim},
		{"persistentvolume", witness.PV, &pv},
		{"volumes.longhorn.io", witness.Volume, &volume},
		{"poddisruptionbudget", witness.Budget, &budget},
	} {
		current, err := c.get(ctx, resource.kind, resource.identity.Namespace, resource.identity.Name, false)
		if err != nil || benchmarkIdentity(current) != resource.identity {
			return refused
		}
		*resource.dest = current
	}
	pods, err := c.list(ctx, "pods", "umami")
	pod := cnpgRecoveryPodByName(pods, witness.Consumer.Name)
	if err != nil || benchmarkIdentity(pod) != witness.Consumer ||
		text(pod, "metadata", "resourceVersion") != witness.ConsumerVersion ||
		text(budget, "metadata", "resourceVersion") != witness.BudgetVersion ||
		!umamiV2Transition(cluster) || text(cluster, "status", "currentPrimary") == witness.Consumer.Name ||
		verifyPostgresPod(pod, pod, cluster) != nil || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "replica" ||
		!recoveredPostgresMount(pod, claim) || !onlyCNPGStandbyUsesClaim(pods, pod, witness.Claim.Name, witness.Consumer.Namespace) ||
		!umamiStandbyDisruptionAllowed(budget, cluster) || !umamiV1StorageRetained(cluster, pod, claim, pv, volume) {
		return refused
	}
	return nil
}
