package main

import (
	"errors"
	"fmt"
)

// bindJournalHistory reads the exact retained Helm revision captured by the
// durable retirement journal. Once native uninstall has completed, Helm status
// is not a stable terminal-state API across Helm storage drivers and versions;
// the immutable Kubernetes Secret identity is the stronger source of truth.
// This path is enabled only by a parsed journal UID and keeps the complete
// stored-content and preservation checks that run after collection.
func bindJournalHistory(s *snapshot, historyUID string) error {
	failure := errors.New("journal-bound release history unverified")
	items, err := members(s.inventory)
	if err != nil || historyUID == "" {
		return failure
	}
	var stored map[string]any
	for _, secret := range items {
		if field(secret, "metadata", "uid") != historyUID {
			continue
		}
		owners, ownerErr := ownerReferences(secret)
		if stored != nil || ownerErr != nil || len(owners) != 0 || !metadataValid(secret) || secret["apiVersion"] != "v1" || secret["kind"] != "Secret" || field(secret, "metadata", "namespace") != "backstage" || secret["type"] != "helm.sh/release.v1" || field(secret, "metadata", "labels", "owner") != "helm" || field(secret, "metadata", "labels", "name") != "backstage" || field(secret, "metadata", "labels", "status") != "uninstalled" {
			return failure
		}
		stored, err = storedRelease(secret)
		if err != nil {
			return failure
		}
		version := fmt.Sprint(stored["version"])
		if stored["name"] != "backstage" || stored["namespace"] != "backstage" || integer(stored["version"]) < 1 || field(stored, "info", "status") != "uninstalled" || field(secret, "metadata", "name") != "sh.helm.release.v1.backstage.v"+version || field(secret, "metadata", "labels", "version") != version || text(field(stored, "chart", "metadata", "name")) == "" || text(field(stored, "chart", "metadata", "version")) == "" {
			return failure
		}
	}
	if stored == nil {
		return failure
	}
	s.release = stored
	return nil
}

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
