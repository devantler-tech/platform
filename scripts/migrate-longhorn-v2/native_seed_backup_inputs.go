package main

import "time"

func verifySeedBackupInputs(location object, schedules []object, now time.Time) (benchmarkResource, string, error) {
	if now.IsZero() || text(location, "apiVersion") != "velero.io/v1" || text(location, "kind") != "BackupStorageLocation" ||
		text(location, "metadata", "name") != "default" || !seedVeleroIdentity(location) ||
		text(location, "metadata", "annotations", "meta.helm.sh/release-name") != "velero" || text(location, "metadata", "annotations", "meta.helm.sh/release-namespace") != "velero" ||
		value(location, "spec", "default") != true || text(location, "spec", "accessMode") != "ReadWrite" || text(location, "spec", "provider") != "aws" ||
		text(location, "spec", "objectStorage", "bucket") == "" || text(location, "spec", "objectStorage", "prefix") == "" || text(location, "status", "phase") != "Available" {
		return benchmarkResource{}, "", errSeedEvidence
	}
	validated, err := time.Parse(time.RFC3339Nano, text(location, "status", "lastValidationTime"))
	if err != nil || validated.After(now) || now.Sub(validated) > 15*time.Minute {
		return benchmarkResource{}, "", errSeedEvidence
	}
	seenNames, seenUIDs, daily := map[string]bool{}, map[string]bool{}, false
	for _, schedule := range schedules {
		name, uid := text(schedule, "metadata", "name"), text(schedule, "metadata", "uid")
		if text(schedule, "apiVersion") != "velero.io/v1" || text(schedule, "kind") != "Schedule" || !seedVeleroIdentity(schedule) ||
			seenNames[name] || seenUIDs[uid] || uid == text(location, "metadata", "uid") {
			return benchmarkResource{}, "", errSeedEvidence
		}
		seenNames[name], seenUIDs[uid] = true, true
		excluded, ok := value(schedule, "spec", "template", "excludedNamespaces").([]any)
		if !ok {
			return benchmarkResource{}, "", errSeedEvidence
		}
		isolated := false
		for _, namespace := range excluded {
			name, ok := namespace.(string)
			if !ok || name == "" {
				return benchmarkResource{}, "", errSeedEvidence
			}
			isolated = isolated || name == benchmarkNamespace
		}
		if !isolated {
			return benchmarkResource{}, "", errSeedEvidence
		}
		daily = daily || name == "velero-daily-full"
	}
	if !daily {
		return benchmarkResource{}, "", errSeedEvidence
	}
	digest, err := seedSpecDigest(location)
	if err != nil {
		return benchmarkResource{}, "", err
	}
	return benchmarkIdentity(location), digest, nil
}

func seedVeleroIdentity(o object) bool {
	return kubeName.MatchString(text(o, "metadata", "name")) && text(o, "metadata", "namespace") == "velero" &&
		text(o, "metadata", "uid") != "" && text(o, "metadata", "resourceVersion") != "" && value(o, "metadata", "deletionTimestamp") == nil
}
