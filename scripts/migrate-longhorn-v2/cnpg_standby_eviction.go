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
		!nativeRestoreUID(pod.UID) || witness.ConsumerVersion == "" || c.context != "admin@prod" || c.run == nil {
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
	return sourceCurrent(ctx, c.run, source)
}
