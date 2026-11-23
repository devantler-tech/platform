package main

import (
	"testing"
	"time"
)

func seedBackupInputFixture() (object, []object, time.Time) {
	now := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)
	location := object{"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation", "metadata": object{"name": "default", "namespace": "velero", "uid": "location-uid", "resourceVersion": "17",
		"annotations": object{"meta.helm.sh/release-name": "velero", "meta.helm.sh/release-namespace": "velero"}},
		"spec":   object{"default": true, "accessMode": "ReadWrite", "provider": "aws", "objectStorage": object{"bucket": "fixture", "prefix": "velero"}},
		"status": object{"phase": "Available", "lastValidationTime": now.Add(-time.Minute).Format(time.RFC3339)}}
	schedule := object{"apiVersion": "velero.io/v1", "kind": "Schedule", "metadata": object{"name": "velero-daily-full", "namespace": "velero", "uid": "schedule-uid", "resourceVersion": "21"},
		"spec": object{"template": object{"includedNamespaces": []any{"*"}, "excludedNamespaces": []any{"kube-system", "velero", benchmarkNamespace}}}}
	return location, []object{schedule}, now
}

func TestNativeSeedRequiresTheExistingWritableStoreAndScheduleIsolation(t *testing.T) {
	for _, fault := range []string{"", "unavailable", "read-only", "other provider", "not default", "missing bucket", "missing prefix", "foreign release", "replacement scope", "missing location UID", "deleting location", "stale validation", "future validation", "missing validation", "no schedules", "daily includes source", "another schedule includes source", "ambiguous exclusion", "missing schedule UID", "deleting schedule"} {
		t.Run(fault, func(t *testing.T) {
			location, schedules, now := seedBackupInputFixture()
			schedule := schedules[0]
			switch fault {
			case "unavailable":
				location["status"].(object)["phase"] = "Unavailable"
			case "read-only":
				location["spec"].(object)["accessMode"] = "ReadOnly"
			case "other provider":
				location["spec"].(object)["provider"] = "other"
			case "not default":
				location["spec"].(object)["default"] = false
			case "missing bucket":
				delete(value(location, "spec", "objectStorage").(object), "bucket")
			case "missing prefix":
				delete(value(location, "spec", "objectStorage").(object), "prefix")
			case "foreign release":
				value(location, "metadata", "annotations").(object)["meta.helm.sh/release-name"] = "foreign"
			case "replacement scope":
				location["metadata"].(object)["namespace"] = "other"
			case "missing location UID":
				delete(location["metadata"].(object), "uid")
			case "deleting location":
				location["metadata"].(object)["deletionTimestamp"] = now.Format(time.RFC3339)
			case "stale validation":
				location["status"].(object)["lastValidationTime"] = now.Add(-16 * time.Minute).Format(time.RFC3339)
			case "future validation":
				location["status"].(object)["lastValidationTime"] = now.Add(time.Minute).Format(time.RFC3339)
			case "missing validation":
				delete(location["status"].(object), "lastValidationTime")
			case "no schedules":
				schedules = nil
			case "daily includes source":
				value(schedule, "spec", "template").(object)["excludedNamespaces"] = []any{"kube-system", "velero"}
			case "another schedule includes source":
				schedules = append(schedules, object{"apiVersion": "velero.io/v1", "kind": "Schedule", "metadata": object{"name": "another", "namespace": "velero", "uid": "another-uid", "resourceVersion": "22"},
					"spec": object{"template": object{"includedNamespaces": []any{"*"}}}})
			case "ambiguous exclusion":
				value(schedule, "spec", "template").(object)["excludedNamespaces"] = []any{benchmarkNamespace, 7}
			case "missing schedule UID":
				delete(schedule["metadata"].(object), "uid")
			case "deleting schedule":
				schedule["metadata"].(object)["deletionTimestamp"] = now.Format(time.RFC3339)
			}
			identity, digest, err := verifySeedBackupInputs(location, schedules, now)
			if (err == nil) != (fault == "") {
				t.Fatal("the source could be written without its original writable destination and isolated schedules", err)
			}
			if fault == "" && (identity != benchmarkIdentity(location) || !seedSHA256.MatchString(digest)) {
				t.Fatal("the original backup destination was not captured")
			}
		})
	}
}

func TestNativeSeedAcceptsOtherNamespaceSchedulesOnlyWithProvenIsolation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template object
		isolated bool
	}{
		{"OpenBao-only schedule", object{"includedNamespaces": []any{"openbao"}, "includeClusterResources": false}, true},
		{"canary included", object{"includedNamespaces": []any{"openbao", benchmarkNamespace}, "includeClusterResources": false}, false},
		{"all namespaces", object{"includedNamespaces": []any{"*"}, "includeClusterResources": false}, false},
		{"namespace pattern", object{"includedNamespaces": []any{"longhorn-*"}, "includeClusterResources": false}, false},
		{"empty include list", object{"includedNamespaces": []any{}, "includeClusterResources": false}, false},
		{"malformed include list", object{"includedNamespaces": []any{"openbao", 1}, "includeClusterResources": false}, false},
		{"cluster scope unspecified", object{"includedNamespaces": []any{"openbao"}}, false},
		{"cluster resources included", object{"includedNamespaces": []any{"openbao"}, "includeClusterResources": true}, false},
		{"additional resource policy", object{"includedNamespaces": []any{"openbao"}, "includeClusterResources": false,
			"resourcePolicy": object{"kind": "configmap", "name": "namespace-filter"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			location, schedules, now := seedBackupInputFixture()
			schedules = append(schedules, object{"apiVersion": "velero.io/v1", "kind": "Schedule",
				"metadata": object{"name": "openbao-recovery-keys", "namespace": "velero", "uid": "keys-uid", "resourceVersion": "22"},
				"spec":     object{"template": tc.template}})
			_, _, err := verifySeedBackupInputs(location, schedules, now)
			if (err == nil) != tc.isolated {
				t.Fatalf("schedule isolation = %t, want %t: %v", err == nil, tc.isolated, err)
			}
		})
	}
}
