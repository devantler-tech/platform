package main

import (
	"context"
	"errors"
)

// observeCNPGMigration is a protected read-only step, not cutover clearance.
func observeCNPGMigration(ctx context.Context, c client, target, source string) (cnpgObservation, error) {
	var selected cnpgSource
	for _, candidate := range cnpgSources() {
		if candidate.Name == target {
			selected = candidate
		}
	}
	if selected.Name == "" {
		return cnpgObservation{}, errors.New("the selected database is outside this migration")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return cnpgObservation{}, err
	}
	before, err := c.readCNPGObservation(ctx, selected)
	if err != nil {
		return cnpgObservation{}, err
	}
	if err = c.readCNPGPhysicalState(ctx, &before); err != nil {
		return cnpgObservation{}, err
	}
	after, err := c.readCNPGObservation(ctx, selected)
	if err != nil {
		return cnpgObservation{}, err
	}
	if err = c.readCNPGPhysicalState(ctx, &after); err != nil {
		return cnpgObservation{}, err
	}
	// The final whole-cluster read follows the final SQL process bracket. A
	// switchover during the last command must not escape through an older read.
	final, err := c.readCNPGObservation(ctx, selected)
	if err != nil {
		return cnpgObservation{}, err
	}
	final.States = after.States
	if err = validateCNPGObservation(before, final); err != nil {
		return cnpgObservation{}, err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return cnpgObservation{}, err
	}
	return final, nil
}

// readCNPGObservation obtains an unfiltered complete namespace Pod inventory.
func (c client) readCNPGObservation(ctx context.Context, selected cnpgSource) (cnpgObservation, error) {
	cluster, err := c.get(ctx, "cluster.postgresql.cnpg.io", selected.Namespace, selected.Name, false)
	if err != nil {
		return cnpgObservation{}, err
	}
	generation, validGeneration := value(cluster, "metadata", "generation").(float64)
	if text(cluster, "metadata", "namespace") != selected.Namespace || text(cluster, "metadata", "name") != selected.Name || !validGeneration || generation < 1 || generation > 2147483647 || generation != float64(int64(generation)) {
		return cnpgObservation{}, errors.New("database response is not bound to the selected current resource")
	}
	pods, err := c.list(ctx, "pods", selected.Namespace)
	if err != nil {
		return cnpgObservation{}, err
	}
	owned, err := healthyCNPGPods(cluster, pods)
	if err != nil {
		return cnpgObservation{}, err
	}
	return cnpgObservation{Cluster: cluster, Pods: owned, States: map[string]postgresState{}}, nil
}

// readCNPGPhysicalState captures the primary barrier before either standby.
func (c client) readCNPGPhysicalState(ctx context.Context, observation *cnpgObservation) error {
	primary := text(observation.Cluster, "status", "currentPrimary")
	ordered := make([]object, 0, 3)
	for _, pod := range observation.Pods {
		if text(pod, "metadata", "name") == primary {
			ordered = append(ordered, pod)
		}
	}
	for _, pod := range observation.Pods {
		if text(pod, "metadata", "name") != primary {
			ordered = append(ordered, pod)
		}
	}
	if len(ordered) != 3 {
		return errors.New("complete physical database coverage is required")
	}
	for _, pod := range ordered {
		state, err := c.observePostgres(ctx, observation.Cluster, pod)
		if err != nil {
			return err
		}
		observation.States[text(pod, "metadata", "name")] = state
	}
	return nil
}
