package main

import (
	"errors"
	"fmt"
)

// Helm v4.2.0 status deliberately omits Chart. Recover it only from the exact
// retained revision already in this snapshot; never infer it from desired state
// or controller history. All existing eligibility and preservation checks still
// run afterward, including the complete stored-content comparison.
func bindStatusChart(s *snapshot) error {
	if s.release["chart"] != nil {
		return nil
	}
	failure := errors.New("stored status chart binding unverified")
	version := integer(s.release["version"])
	if version < 1 || s.release["name"] != "backstage" || s.release["namespace"] != "backstage" {
		return failure
	}
	items, err := members(s.inventory)
	if err != nil {
		return failure
	}
	var stored map[string]any
	for _, secret := range items {
		if secret["kind"] != "Secret" || field(secret, "metadata", "name") != "sh.helm.release.v1.backstage.v"+fmt.Sprint(version) {
			continue
		}
		owners, err := ownerReferences(secret)
		if stored != nil || !metadataValid(secret) || secret["apiVersion"] != "v1" || field(secret, "metadata", "namespace") != "backstage" || secret["type"] != "helm.sh/release.v1" || err != nil || len(owners) != 0 || field(secret, "metadata", "labels", "owner") != "helm" || field(secret, "metadata", "labels", "name") != "backstage" || field(secret, "metadata", "labels", "version") != fmt.Sprint(version) || field(secret, "metadata", "labels", "status") != field(s.release, "info", "status") {
			return failure
		}
		stored, err = storedRelease(secret)
		if err != nil || stored["name"] != s.release["name"] || stored["namespace"] != s.release["namespace"] || stored["version"] != s.release["version"] || field(stored, "info", "status") != field(s.release, "info", "status") || text(field(stored, "chart", "metadata", "name")) == "" || text(field(stored, "chart", "metadata", "version")) == "" {
			return failure
		}
	}
	if stored == nil {
		return failure
	}
	s.release["chart"] = stored["chart"]
	if !sameStoredContent(stored, s.release) {
		return failure
	}
	return nil
}
