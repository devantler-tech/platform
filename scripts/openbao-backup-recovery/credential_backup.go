package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type credentialBackup struct {
	Name       string
	Start, End time.Time
}

// selectCredentialBackup requires a completed backup covering the fixed recovery resource.
func selectCredentialBackup(raw []byte, now time.Time) (credentialBackup, error) {
	var result credentialBackup
	canonical := map[string][]string{
		"": {"apiVersion", "kind", "items"}, "items[]": {"apiVersion", "kind", "metadata", "spec", "status"},
		"items[].metadata": {"name", "namespace", "uid", "deletionTimestamp"},
		"items[].spec":     {"storageLocation", "includedNamespaces", "excludedNamespaces", "includedResources", "excludedResources", "includedNamespaceScopedResources", "excludedNamespaceScopedResources"},
		"items[].status":   {"phase", "formatVersion", "startTimestamp", "completionTimestamp"},
	}
	if len(raw) == 0 || len(raw) > 1<<20 || strictjson.Validate(raw, canonical) != nil {
		return result, errEvidence
	}
	var list struct {
		APIVersion, Kind string
		Items            []struct {
			APIVersion, Kind string
			Metadata         struct {
				Name, Namespace, UID string
				DeletionTimestamp    *string
			}
			Spec struct {
				StorageLocation                                                              string
				IncludedNamespaces, ExcludedNamespaces, IncludedResources, ExcludedResources []string
				IncludedNamespaceScopedResources, ExcludedNamespaceScopedResources           []string
			}
			Status struct {
				Phase, FormatVersion                string
				StartTimestamp, CompletionTimestamp time.Time
			}
		}
	}
	if json.Unmarshal(raw, &list) != nil {
		return result, errEvidence
	}
	validList := (list.Kind == "BackupList" && list.APIVersion == "velero.io/v1") || (list.Kind == "List" && list.APIVersion == "v1")
	if len(list.Items) == 0 || len(list.Items) > 256 || !validList {
		return result, errEvidence
	}
	includes := func(values []string, value string) bool {
		return len(values) == 0 || slices.Contains(values, "*") || slices.Contains(values, value)
	}
	excludes := func(values []string, value string) bool {
		return slices.Contains(values, "*") || slices.Contains(values, value)
	}
	seenNames, seenUIDs := map[string]bool{}, map[string]bool{}
	ambiguous := false
	for _, item := range list.Items {
		if item.Status.Phase != "Completed" || item.Metadata.DeletionTimestamp != nil {
			continue
		}
		if item.Kind != "Backup" || item.APIVersion != "velero.io/v1" || item.Metadata.Namespace != "velero" ||
			!regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`).MatchString(item.Metadata.Name) ||
			!regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`).MatchString(item.Metadata.UID) ||
			seenNames[item.Metadata.Name] || seenUIDs[item.Metadata.UID] {
			return credentialBackup{}, errEvidence
		}
		seenNames[item.Metadata.Name], seenUIDs[item.Metadata.UID] = true, true
		if (item.Spec.StorageLocation != "" && item.Spec.StorageLocation != "default") ||
			!includes(item.Spec.IncludedNamespaces, "openbao") || excludes(item.Spec.ExcludedNamespaces, "openbao") ||
			!includes(item.Spec.IncludedResources, "secrets") || excludes(item.Spec.ExcludedResources, "secrets") ||
			!includes(item.Spec.IncludedNamespaceScopedResources, "secrets") || excludes(item.Spec.ExcludedNamespaceScopedResources, "secrets") {
			continue
		}
		start, end := item.Status.StartTimestamp, item.Status.CompletionTimestamp
		if item.Status.FormatVersion != "1.1.0" || start.IsZero() || end.IsZero() || end.Before(start) || end.After(now.Add(5*time.Minute)) {
			return credentialBackup{}, errEvidence
		}
		// The data snapshot retains its separate 24-hour job-freshness guard.
		// Recovery credentials may come from any still-retained matching generation.
		if start.Before(now.Add(-14 * 24 * time.Hour)) {
			continue
		}
		if end.Equal(result.End) {
			ambiguous = true
		}
		if result.Name == "" || end.After(result.End) {
			result = credentialBackup{Name: item.Metadata.Name, Start: start, End: end}
			ambiguous = false
		}
	}
	if result.Name == "" || ambiguous {
		return result, errEvidence
	}
	return result, nil
}
