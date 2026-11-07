package main

import (
	"errors"
	"reflect"
)

type cnpgObservation struct {
	Cluster object
	Pods    []object
	States  map[string]postgresState
}

// validateCNPGObservation requires physical replay and stable process identities.
func validateCNPGObservation(before, after cnpgObservation) error {
	refused := errors.New("database identity, process or physical replication changed during observation")
	original, err := healthyCNPGPods(before.Cluster, before.Pods)
	if err != nil {
		return err
	}
	current, err := healthyCNPGPods(after.Cluster, after.Pods)
	if err != nil {
		return err
	}
	if text(before.Cluster, "metadata", "uid") != text(after.Cluster, "metadata", "uid") ||
		!reflect.DeepEqual(value(before.Cluster, "spec"), value(after.Cluster, "spec")) ||
		value(before.Cluster, "metadata", "generation") != value(after.Cluster, "metadata", "generation") ||
		text(before.Cluster, "status", "currentPrimary") != text(after.Cluster, "status", "currentPrimary") ||
		len(before.States) != 3 || len(after.States) != 3 {
		return refused
	}
	primaryName := text(before.Cluster, "status", "currentPrimary")
	barrier, exists := before.States[primaryName]
	if !exists || barrier.InRecovery || validPostgresState(barrier) != nil {
		return refused
	}
	for i, pod := range original {
		name := text(pod, "metadata", "name")
		if text(current[i], "metadata", "name") != name || verifyPostgresPod(current[i], pod, before.Cluster) != nil {
			return refused
		}
		initial, present := before.States[name]
		latest, observed := after.States[name]
		if !present || !observed || validPostgresState(initial) != nil || validPostgresState(latest) != nil || initial.SystemID != latest.SystemID || initial.Timeline != latest.Timeline || initial.InRecovery != latest.InRecovery ||
			initial.PostmasterStart == "" || initial.PostmasterStart != latest.PostmasterStart {
			return refused
		}
		if name == primaryName {
			oldPosition, _ := walPosition(initial.FlushLSN)
			newPosition, _ := walPosition(latest.FlushLSN)
			if latest.InRecovery || newPosition < oldPosition {
				return refused
			}
		} else {
			if verifyPostgresStandby(barrier, initial) != nil || verifyPostgresStandby(barrier, latest) != nil {
				return refused
			}
			oldPosition, _ := walPosition(initial.ReplayLSN)
			newPosition, _ := walPosition(latest.ReplayLSN)
			if newPosition < oldPosition {
				return refused
			}
		}
	}
	return nil
}
