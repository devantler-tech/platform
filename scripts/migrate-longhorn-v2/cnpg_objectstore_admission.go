package main

import (
	"errors"
	"net/url"
)

// matchRecoveryObjectStoreAdmission accepts the complete read-only archive
// projection under the independently bound Barman 0.16.0 schema. That schema
// adds no defaults to this proposal: its optional sidecar parent is absent.
// This comparator establishes no source identity or creation authority.
func matchRecoveryObjectStoreAdmission(proposed, admitted object) (object, error) {
	refused := errors.New("server admission changed the isolated archive recovery projection")
	run := text(proposed, "metadata", "labels", "storage.platform.devantler.tech/recovery-run")
	namespace := text(proposed, "metadata", "namespace")
	target := ""
	for _, source := range cnpgArchiveSources() {
		if namespace == "longhorn-db-proof-"+source.Name+"-"+run {
			target = source.Name
		}
	}
	if target == "" || !recoveryRun.MatchString(run) || !isolationNamespace.MatchString(namespace) || text(proposed, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" {
		return nil, refused
	}
	for _, o := range []object{proposed, admitted} {
		if text(o, "apiVersion") != "barmancloud.cnpg.io/v1" || text(o, "kind") != "ObjectStore" || text(o, "metadata", "name") != "restore-source" || text(o, "metadata", "namespace") != namespace || value(o, "metadata", "deletionTimestamp") != nil {
			return nil, refused
		}
		for _, field := range []string{"annotations", "ownerReferences", "finalizers"} {
			if value(o, "metadata", field) != nil {
				return nil, refused
			}
		}
	}
	if !recoveryJSONEqual(value(proposed, "metadata", "labels"), value(admitted, "metadata", "labels")) {
		return nil, refused
	}
	spec, err := cloneRecoverySpec(value(proposed, "spec"))
	if err != nil || len(spec) != 1 {
		return nil, refused
	}
	configuration, ok := value(spec, "configuration").(object)
	if !ok || len(configuration) != 5 || !recoveryEndpoint.MatchString(text(configuration, "endpointURL")) || !recoveryJSONEqual(value(configuration, "data"), object{"compression": "gzip"}) || !recoveryJSONEqual(value(configuration, "wal"), object{"compression": "gzip"}) {
		return nil, refused
	}
	destination, err := url.Parse(text(configuration, "destinationPath"))
	if err != nil || destination.Scheme != "s3" || !recoveryBucket.MatchString(destination.Host) || destination.User != nil || destination.Path != "/cnpg/"+target || destination.RawPath != "" || destination.RawQuery != "" || destination.ForceQuery || destination.Fragment != "" {
		return nil, refused
	}
	credentials := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		credentials[field] = object{"name": "archive-read", "key": key}
	}
	if !recoveryJSONEqual(value(configuration, "s3Credentials"), credentials) || !recoveryJSONEqual(spec, value(admitted, "spec")) {
		return nil, refused
	}
	return spec, nil
}
