package main

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// credentialArchivePrefix binds remote reads to the effective declared backup location.
func credentialArchivePrefix(base, cluster, variables, raw []byte, target target) (string, error) {
	values := map[string]string{}
	for _, input := range []struct {
		raw        []byte
		kind, name string
	}{{base, "ConfigMap", "variables-base"}, {cluster, "ConfigMap", "variables-cluster"}, {variables, "Secret", "variables-cluster"}} {
		data, err := fixedObject(input.raw, input.kind, "flux-system", input.name)
		if err != nil {
			return "", errEvidence
		}
		for name, value := range data {
			values[name] = value
		}
	}
	prefix := strings.TrimSuffix(values["r2_prefix_velero"], "/")
	if !regexp.MustCompile(`^velero/[a-z0-9][a-z0-9-]{0,62}$`).MatchString(prefix) {
		return "", errEvidence
	}
	canonical := map[string][]string{"": {"apiVersion", "kind", "metadata", "spec", "status"}, "metadata": {"name", "namespace"}, "spec": {"provider", "objectStorage", "config"}, "spec.objectStorage": {"bucket", "prefix"}, "status": {"phase"}}
	if len(raw) == 0 || len(raw) > 1<<20 || strictjson.Validate(raw, canonical) != nil {
		return "", errEvidence
	}
	var location struct {
		APIVersion, Kind string
		Metadata         struct{ Name, Namespace string }
		Spec             struct {
			Provider      string
			ObjectStorage struct{ Bucket, Prefix string }
			Config        map[string]string
		}
		Status struct{ Phase string }
	}
	if json.Unmarshal(raw, &location) != nil || location.APIVersion != "velero.io/v1" || location.Kind != "BackupStorageLocation" ||
		location.Metadata.Name != "default" || location.Metadata.Namespace != "velero" || location.Status.Phase != "Available" ||
		location.Spec.Provider != "aws" || location.Spec.ObjectStorage.Bucket != target.Bucket || strings.TrimSuffix(location.Spec.ObjectStorage.Prefix, "/") != prefix ||
		strings.TrimSuffix(location.Spec.Config["s3Url"], "/") != target.Endpoint || location.Spec.Config["region"] != "auto" || location.Spec.Config["s3ForcePathStyle"] != "true" {
		return "", errEvidence
	}
	return prefix, nil
}
