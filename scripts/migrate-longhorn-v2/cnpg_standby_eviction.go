package main

import (
	"context"
	"encoding/json"
	"errors"
)

// Ask the Kubernetes Eviction API to exercise admission and its PDB checks for
// the exact observed standby with persistence disabled at both API boundaries.
func verifyUmamiStandbyEvictionAdmission(ctx context.Context, c client, source string, witness cnpgStandbyCutover) error {
	if !validUmamiEvictionTarget(c, witness) {
		return errors.New("the Umami eviction target is unverified")
	}
	if err := recheckUmamiStandbyAdmission(ctx, c, witness); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if err := requestUmamiStandbyEviction(ctx, c, witness, true); err != nil {
		return err
	}
	if err := recheckUmamiStandbyAdmission(ctx, c, witness); err != nil {
		return err
	}
	return sourceCurrent(ctx, c.run, source)
}

func requestUmamiStandbyEviction(ctx context.Context, c client, witness cnpgStandbyCutover, dryRun bool) error {
	const namespace = "umami"
	pod := witness.Consumer
	refused := errors.New("the exact standby did not pass Kubernetes eviction admission")
	if !validUmamiEvictionTarget(c, witness) {
		return refused
	}
	path := "/api/v1/namespaces/" + namespace + "/pods/" + pod.Name + "/eviction"
	options := object{"preconditions": object{"uid": pod.UID, "resourceVersion": witness.ConsumerVersion}}
	if dryRun {
		options["dryRun"] = []string{"All"}
		path += "?dryRun=All"
	}
	input := object{"apiVersion": "policy/v1", "kind": "Eviction",
		"metadata": object{"name": pod.Name, "namespace": namespace}, "deleteOptions": options}
	encoded, err := json.Marshal(input)
	if err != nil {
		return refused
	}
	raw, err := c.kubectl(ctx, encoded, "create", "--raw", path, "-f", "-")
	var status object
	if err != nil || json.Unmarshal(raw, &status) != nil || text(status, "apiVersion") != "v1" ||
		text(status, "kind") != "Status" || text(status, "status") != "Success" {
		return refused
	}
	return nil
}

func validUmamiEvictionTarget(c client, witness cnpgStandbyCutover) bool {
	pod := witness.Consumer
	_, err := cnpgInstanceID(pod.Name, "umami-db")
	return err == nil && pod.Namespace == "umami" && nativeRestoreUID(pod.UID) &&
		witness.ConsumerVersion != "" && witness.BudgetVersion != "" && c.context == "admin@prod" && c.run != nil
}

// A dry-run success can also mean that no budget matched. Check the same
// budget revision, pod, retained volume and sole consumer on both sides.
func recheckUmamiStandbyAdmission(ctx context.Context, c client, witness cnpgStandbyCutover) error {
	actual, err := readUmamiStandbyAdmission(ctx, c, witness)
	if err != nil {
		return err
	}
	if !umamiV1StorageRetained(actual.cluster, actual.pod, actual.claim, actual.pv, actual.volume) {
		return errors.New("the original standby storage changed during admission")
	}
	return nil
}

type umamiStandbyAdmission struct {
	cluster, pod, claim, pv, volume, budget object
}

func readUmamiStandbyAdmission(ctx context.Context, c client, witness cnpgStandbyCutover) (umamiStandbyAdmission, error) {
	refused := errors.New("the standby, disruption budget or retained storage changed during admission")
	var actual umamiStandbyAdmission
	for _, resource := range []struct {
		kind     string
		identity benchmarkResource
		dest     *object
	}{
		{"clusters.postgresql.cnpg.io", witness.Database, &actual.cluster},
		{"persistentvolumeclaim", witness.Claim, &actual.claim},
		{"persistentvolume", witness.PV, &actual.pv},
		{"volumes.longhorn.io", witness.Volume, &actual.volume},
		{"poddisruptionbudget", witness.Budget, &actual.budget},
	} {
		current, err := c.get(ctx, resource.kind, resource.identity.Namespace, resource.identity.Name, false)
		if err != nil || benchmarkIdentity(current) != resource.identity {
			return umamiStandbyAdmission{}, refused
		}
		*resource.dest = current
	}
	pods, err := c.list(ctx, "pods", "umami")
	actual.pod = cnpgRecoveryPodByName(pods, witness.Consumer.Name)
	pod, cluster, budget := actual.pod, actual.cluster, actual.budget
	if err != nil || benchmarkIdentity(pod) != witness.Consumer ||
		text(pod, "metadata", "resourceVersion") != witness.ConsumerVersion ||
		text(budget, "metadata", "resourceVersion") != witness.BudgetVersion ||
		!umamiV2Transition(cluster) || text(cluster, "status", "currentPrimary") == witness.Consumer.Name ||
		verifyPostgresPod(pod, pod, cluster) != nil || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "replica" ||
		!recoveredPostgresMount(pod, actual.claim) || !onlyCNPGStandbyUsesClaim(pods, pod, witness.Claim.Name, witness.Consumer.Namespace) ||
		!umamiStandbyDisruptionAllowed(budget, cluster) {
		return umamiStandbyAdmission{}, refused
	}
	return actual, nil
}
