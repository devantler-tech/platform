package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

type cnpgAdmissionEnvironment struct {
	CRD                                    cnpgCRDBinding
	DeploymentUID, DeploymentSHA           string
	DeploymentGeneration                   uint64
	ConfigurationUID, ConfigurationVersion string
	Operators                              []cnpgOperatorProcess
	Defaults                               recoveryAdmissionDefaults
	Startup                                []cnpgStartupBinding
}

// observeCNPGAdmissionEnvironment uses the compiled release pin, never a user input.
func observeCNPGAdmissionEnvironment(ctx context.Context, c client) (cnpgAdmissionEnvironment, error) {
	return collectCNPGAdmissionEnvironment(ctx, c, cnpgReviewedCRDSHA)
}

// collectCNPGAdmissionEnvironment brackets startup evidence with complete native inventories.
// The schema argument is a compiled caller pin; only tests substitute a synthetic profile.
func collectCNPGAdmissionEnvironment(ctx context.Context, c client, schemaSHA string) (result cnpgAdmissionEnvironment, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = cnpgAdmissionEnvironment{}
			err = fmt.Errorf("database operator environment could not be bound: phase=%s", phase)
		}
	}()
	if c.context != "admin@prod" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(schemaSHA) {
		return result, errors.New("operator environment requires the fixed protected context and reviewed pin")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	phase = "before"
	before, err := readCNPGOperatorSnapshot(ctx, c, schemaSHA)
	if err != nil {
		return result, err
	}
	phase = "startup"
	startup := make([]cnpgStartupBinding, 0, 2)
	for _, process := range before.Operators {
		raw, e := c.kubectl(ctx, nil, "logs", process.Pod, "--namespace=cnpg-system", "--container=manager", "--limit-bytes=131072")
		if e != nil {
			return result, e
		}
		bound, e := bindCNPGOperatorStartup(raw, process.StartedAt)
		if e != nil {
			return result, e
		}
		if len(startup) > 0 && (bound.Defaults != startup[0].Defaults || bound.ConfigSHA != startup[0].ConfigSHA) {
			return result, errors.New("operator startup configurations disagree")
		}
		startup = append(startup, bound)
	}
	phase = "after"
	after, err := readCNPGOperatorSnapshot(ctx, c, schemaSHA)
	if err != nil {
		return result, err
	}
	phase = "continuity"
	if !recoveryJSONEqual(before, after) || len(startup) != 2 || ctx.Err() != nil {
		return result, errors.New("operator environment changed during collection")
	}
	before.Defaults = startup[0].Defaults
	before.Startup = startup
	return before, nil
}

// readCNPGOperatorSnapshot resolves all manager Pods through complete native inventories.
func readCNPGOperatorSnapshot(ctx context.Context, c client, schemaSHA string) (cnpgAdmissionEnvironment, error) {
	refused := errors.New("complete reviewed operator environment is not available")
	raw, err := c.kubectl(ctx, nil, "get", "customresourcedefinition", "clusters.postgresql.cnpg.io", "--output=json")
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	schema, err := bindCNPGCRD(raw, schemaSHA)
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	deployment, err := c.get(ctx, "deployment", "cnpg-system", "cloudnative-pg", false)
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	cm, err := c.get(ctx, "configmap", "cnpg-system", "cnpg-controller-manager-config", false)
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	if text(cm, "apiVersion") != "v1" || text(cm, "kind") != "ConfigMap" || text(cm, "metadata", "name") != "cnpg-controller-manager-config" || text(cm, "metadata", "namespace") != "cnpg-system" || value(cm, "metadata", "deletionTimestamp") != nil {
		return cnpgAdmissionEnvironment{}, refused
	}
	for _, field := range []string{"data", "binaryData"} {
		if raw := cm[field]; raw != nil {
			content, ok := raw.(object)
			if !ok || len(content) != 0 {
				return cnpgAdmissionEnvironment{}, refused
			}
		}
	}
	// This recovery profile permits no optional copied registry credential.
	// Successful native absence is required in both environment observations.
	pull, err := c.get(ctx, "secret", "cnpg-system", "cnpg-pull-secret", true)
	if err != nil || pull != nil {
		return cnpgAdmissionEnvironment{}, refused
	}
	replicas, err := c.list(ctx, "replicasets", "cnpg-system")
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	pods, err := c.list(ctx, "pods", "cnpg-system")
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	nodes, err := c.list(ctx, "nodes", "")
	if err != nil {
		return cnpgAdmissionEnvironment{}, err
	}
	nodeByName := map[string]object{}
	for _, node := range nodes {
		nodeByName[text(node, "metadata", "name")] = node
	}
	processes := make([]cnpgOperatorProcess, 0, 2)
	seenUID, seenContainer := map[string]bool{}, map[string]bool{}
	for _, pod := range pods {
		candidate := text(pod, "metadata", "labels", "app.kubernetes.io/name") == "cloudnative-pg"
		containers, _ := value(pod, "spec", "containers").([]any)
		for _, rawContainer := range containers {
			container, _ := rawContainer.(object)
			candidate = candidate || text(container, "name") == "manager"
		}
		var parent object
		for _, rs := range replicas {
			if cnpgOperatorOwner(pod, "ReplicaSet", rs) && cnpgOperatorOwner(rs, "Deployment", deployment) {
				if parent != nil {
					return cnpgAdmissionEnvironment{}, refused
				}
				parent = rs
				candidate = true
			}
		}
		if !candidate {
			continue
		}
		if parent == nil {
			return cnpgAdmissionEnvironment{}, refused
		}
		process, e := bindCNPGOperatorProcess(pod, deployment, parent, nodeByName[text(pod, "spec", "nodeName")])
		if e != nil || seenUID[process.PodUID] || seenContainer[process.Container] {
			return cnpgAdmissionEnvironment{}, refused
		}
		seenUID[process.PodUID], seenContainer[process.Container] = true, true
		processes = append(processes, process)
	}
	if len(processes) != 2 {
		return cnpgAdmissionEnvironment{}, refused
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].Pod < processes[j].Pod })
	spec, err := json.Marshal(value(deployment, "spec"))
	if err != nil {
		return cnpgAdmissionEnvironment{}, refused
	}
	generation, ok := nativeObservationUint(value(deployment, "metadata", "generation"))
	if !ok {
		return cnpgAdmissionEnvironment{}, refused
	}
	return cnpgAdmissionEnvironment{CRD: schema, DeploymentUID: text(deployment, "metadata", "uid"), DeploymentSHA: fmt.Sprintf("%x", sha256.Sum256(spec)), DeploymentGeneration: generation, ConfigurationUID: text(cm, "metadata", "uid"), ConfigurationVersion: text(cm, "metadata", "resourceVersion"), Operators: processes}, nil
}
