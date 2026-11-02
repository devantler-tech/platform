// Reads only fixed production objects; raw responses never become public output.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"time"
)

const maxResponse = 1 << 20

type recoveryEvidence struct {
	Configuration, Deployment, Cron []byte
	Peers                           [2][]byte
}
type objectMetadata struct {
	Name, Namespace   string
	Generation        int64
	DeletionTimestamp *string
}
type deploymentObservation struct {
	Metadata objectMetadata
	Spec     struct {
		Replicas int
		Template struct {
			Metadata struct{ Annotations map[string]string }
		}
	}
	Status struct {
		ObservedGeneration                                                               int64
		Replicas, UpdatedReplicas, ReadyReplicas, AvailableReplicas, UnavailableReplicas int
		Conditions                                                                       []struct{ Type, Status, Reason, LastUpdateTime string }
	}
}
type cronObservation struct {
	Metadata objectMetadata
	Spec     struct{ Suspend bool }
	Status   struct{ LastSuccessfulTime string }
}
type alertObservation struct {
	Labels map[string]string
	Status struct{ State string }
}

func decodeObservation(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > maxResponse || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("empty or oversized observation")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return errors.New("malformed observation")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("multiple observation documents")
	}
	return nil
}

func assessRecovery(e recoveryEvidence, expected map[string]string, checksum string, now time.Time) (int, error) {
	var config struct {
		Metadata objectMetadata
		Data     map[string]string
	}
	if err := decodeObservation(e.Configuration, &config); err != nil {
		return 0, err
	}
	if config.Metadata.Name != "crossplane-sync-exporter" || config.Metadata.Namespace != "observability" ||
		config.Metadata.DeletionTimestamp != nil || len(expected) == 0 || !maps.Equal(config.Data, expected) {
		return 0, errors.New("configuration not bound to reviewed source")
	}
	var deployment deploymentObservation
	if err := decodeObservation(e.Deployment, &deployment); err != nil {
		return 0, err
	}
	if deployment.Metadata.Name != "crossplane-sync-exporter" || deployment.Metadata.Namespace != "observability" ||
		deployment.Metadata.DeletionTimestamp != nil || deployment.Metadata.Generation < 1 || deployment.Status.ObservedGeneration != deployment.Metadata.Generation ||
		deployment.Spec.Replicas != 1 || deployment.Status.Replicas != 1 || deployment.Status.UpdatedReplicas != 1 ||
		deployment.Status.ReadyReplicas != 1 || deployment.Status.AvailableReplicas != 1 || deployment.Status.UnavailableReplicas != 0 ||
		checksum == "" || deployment.Spec.Template.Metadata.Annotations["platform.devantler.tech/config-checksum"] != checksum {
		return 0, errors.New("reviewed exporter rollout not ready")
	}
	var rollout time.Time
	progressConditions := 0
	availableConditions := 0
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == "Available" {
			availableConditions++
			if condition.Status != "True" {
				return 0, errors.New("exporter unavailable")
			}
			continue
		}
		if condition.Type != "Progressing" {
			continue
		}
		progressConditions++
		if condition.Status != "True" || condition.Reason != "NewReplicaSetAvailable" {
			return 0, errors.New("rollout incomplete")
		}
		var err error
		rollout, err = time.Parse(time.RFC3339, condition.LastUpdateTime)
		if err != nil {
			return 0, errors.New("rollout time unavailable")
		}
	}
	if progressConditions != 1 || availableConditions != 1 || rollout.IsZero() || rollout.After(now.Add(time.Minute)) {
		return 0, errors.New("rollout observation incomplete")
	}
	var cron cronObservation
	if err := decodeObservation(e.Cron, &cron); err != nil {
		return 0, err
	}
	if cron.Metadata.Name != "crossplane-sync-alerter" || cron.Metadata.Namespace != "observability" || cron.Metadata.DeletionTimestamp != nil || cron.Spec.Suspend {
		return 0, errors.New("coverage checker unavailable")
	}
	completed, err := time.Parse(time.RFC3339, cron.Status.LastSuccessfulTime)
	if err != nil || completed.Before(rollout) || completed.Before(now.Add(-7*time.Minute)) || completed.After(now.Add(time.Minute)) {
		return 0, errors.New("coverage checker has no fresh post-rollout success")
	}
	count := 0
	for _, raw := range e.Peers {
		var alerts []alertObservation
		if err := decodeObservation(raw, &alerts); err != nil {
			return 0, err
		}
		if alerts == nil || len(alerts) > 500 {
			return 0, errors.New("incomplete peer alert list")
		}
		for _, alert := range alerts {
			if alert.Labels["alertname"] == "" {
				return 0, errors.New("alert identity unavailable")
			}
			switch alert.Status.State {
			case "active", "suppressed", "unprocessed":
			default:
				return 0, errors.New("unknown peer alert state")
			}
			// Silences and inhibition do not establish recovery. Any retained alert
			// from this component is a blocker, regardless of its notification state.
			if strings.HasPrefix(alert.Labels["alertname"], "Crossplane") {
				count++
			}
		}
	}
	return count, nil
}
