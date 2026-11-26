package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type cnpgBarmanEnvironment struct {
	CRD                  cnpgCRDBinding
	DeploymentGeneration uint64
	Operator             cnpgOperatorProcess
}

// observeBarmanEnvironment uses the complete released schema pin, never a live-selected hash.
func observeBarmanEnvironment(ctx context.Context, c client) (cnpgBarmanEnvironment, error) {
	return collectBarmanEnvironment(ctx, c, barmanReviewedCRDSHA)
}

// collectBarmanEnvironment binds only the compiled release; tests substitute a synthetic schema pin.
func collectBarmanEnvironment(ctx context.Context, c client, schemaSHA string) (result cnpgBarmanEnvironment, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = cnpgBarmanEnvironment{}
			err = fmt.Errorf("database archive environment could not be bound: phase=%s", phase)
		}
	}()
	if c.context != "admin@prod" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(schemaSHA) {
		return result, errors.New("archive environment requires the protected context and reviewed release")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	phase = "before"
	before, err := readBarmanSnapshot(ctx, c, schemaSHA)
	if err != nil {
		return result, err
	}
	phase = "after"
	after, err := readBarmanSnapshot(ctx, c, schemaSHA)
	if err != nil {
		return result, err
	}
	phase = "continuity"
	if !recoveryJSONEqual(before, after) || ctx.Err() != nil {
		return result, errors.New("archive process or configuration changed during collection")
	}
	return before, nil
}

// readBarmanSnapshot checks complete native process inventories and the immutable loaded env.
func readBarmanSnapshot(ctx context.Context, c client, schemaSHA string) (cnpgBarmanEnvironment, error) {
	refused := errors.New("complete pinned archive process is not available")
	raw, err := c.kubectl(ctx, nil, "get", "customresourcedefinition", "objectstores.barmancloud.cnpg.io", "--output=json")
	if err != nil {
		return cnpgBarmanEnvironment{}, err
	}
	schema, err := bindCNPGRecoveryCRD(raw, "objectstores.barmancloud.cnpg.io", schemaSHA)
	if err != nil {
		return cnpgBarmanEnvironment{}, err
	}
	deployment, err := c.get(ctx, "deployment", "cnpg-system", "plugin-barman-cloud", false)
	if err != nil {
		return cnpgBarmanEnvironment{}, err
	}
	replicas, err := c.list(ctx, "replicasets", "cnpg-system")
	if err != nil {
		return cnpgBarmanEnvironment{}, err
	}
	pods, err := c.list(ctx, "pods", "cnpg-system")
	if err != nil {
		return cnpgBarmanEnvironment{}, err
	}
	nodes, err := c.list(ctx, "nodes", "")
	if err != nil {
		return cnpgBarmanEnvironment{}, err
	}
	nodeByName := map[string]object{}
	for _, node := range nodes {
		nodeByName[text(node, "metadata", "name")] = node
	}
	var process cnpgOperatorProcess
	count := 0
	for _, pod := range pods {
		candidate := text(pod, "metadata", "labels", "app.kubernetes.io/name") == "plugin-barman-cloud"
		containers, _ := value(pod, "spec", "containers").([]any)
		for _, rawContainer := range containers {
			container, _ := rawContainer.(object)
			candidate = candidate || text(container, "name") == "barman-cloud"
		}
		var parent object
		for _, rs := range replicas {
			if cnpgOperatorOwner(pod, "ReplicaSet", rs) && cnpgOperatorOwner(rs, "Deployment", deployment) {
				if parent != nil {
					return cnpgBarmanEnvironment{}, refused
				}
				parent, candidate = rs, true
			}
		}
		if !candidate {
			continue
		}
		if parent == nil {
			return cnpgBarmanEnvironment{}, refused
		}
		process, err = bindBarmanOperatorProcess(pod, deployment, parent, nodeByName[text(pod, "spec", "nodeName")])
		if err != nil {
			return cnpgBarmanEnvironment{}, err
		}
		count++
	}
	if count != 1 {
		return cnpgBarmanEnvironment{}, refused
	}
	generation, ok := nativeObservationUint(value(deployment, "metadata", "generation"))
	if !ok {
		return cnpgBarmanEnvironment{}, refused
	}
	return cnpgBarmanEnvironment{CRD: schema, DeploymentGeneration: generation, Operator: process}, nil
}
