package namespaceinventory

import (
	"errors"
	"regexp"
)

// emailRecoveryPattern is the namespace of one fresh Email recovery invocation,
// named after its workflow run.
var emailRecoveryPattern = regexp.MustCompile(`^email-data-recovery-[1-9][0-9]{6,19}$`)

// Kubescape's storage serves the scan records it keeps about each namespace's
// objects through one remotely aggregated API. They are derived from objects in
// the namespace and are removed with it. Only this exact registration and these
// record kinds are accepted, and only for an Email recovery namespace.
const (
	scanGroup        = "spdx.softwarecomposition.kubescape.io"
	scanVersion      = "v1beta1"
	scanGroupVersion = scanGroup + "/" + scanVersion
	scanService      = "storage"
	scanNamespace    = "kubescape"
)

// scanRecordKinds maps each accepted record resource to its kind. The storage's
// other namespaced resources (SBOMs, vulnerability manifests and VEX documents)
// describe images and belong to the Kubescape namespace, so an item there is
// refused rather than accepted as a scan record.
var scanRecordKinds = map[string]string{
	"applicationprofiles":                "ApplicationProfile",
	"containerprofiles":                  "ContainerProfile",
	"generatednetworkpolicies":           "GeneratedNetworkPolicy",
	"networkneighborhoods":               "NetworkNeighborhood",
	"vulnerabilitymanifestsummaries":     "VulnerabilityManifestSummary",
	"workloadconfigurationscans":         "WorkloadConfigurationScan",
	"workloadconfigurationscansummaries": "WorkloadConfigurationScanSummary",
}

// scanStorage reports the one accepted remote registration.
func scanStorage(item apiService) bool {
	s := item.Spec.Service
	return item.Metadata.Name == scanVersion+"."+scanGroup && item.Spec.Group == scanGroup && item.Spec.Version == scanVersion &&
		s != nil && s.Name == scanService && s.Namespace == scanNamespace
}

// scanRecordList validates one metadata-only Kubescape collection. The storage
// sets no list resourceVersion, and it omits UIDs on its profile records, so
// these records are bound by resource and name, never adopted as bindings.
// GeneratedNetworkPolicy items are computed views with no stored version; each
// must mirror a NetworkNeighborhood record of the same name.
func scanRecordList(resource servedResource, namespace string, items []partialObject, records map[string]string) error {
	kind, accepted := scanRecordKinds[resource.name]
	if len(items) == 0 {
		return nil
	}
	if !accepted || resource.kind != kind {
		return errors.New("namespace holds a scan record of an unaccepted kind")
	}
	for _, item := range items {
		m := item.Metadata
		key := resource.name + "/" + m.Name
		if item.APIVersion != "meta.k8s.io/v1" || item.Kind != "PartialObjectMetadata" || m.Namespace != namespace || len(m.Name) > 253 || !namePattern.MatchString(m.Name) || len(m.UID) > 128 || (m.ResourceVersion == "" && resource.name != "generatednetworkpolicies") || records[key] != "" {
			return errors.New("namespace scan record identity is incomplete")
		}
		records[key] = kind
	}
	return nil
}

// scanRecordsComplete requires every computed network policy view to mirror a
// stored NetworkNeighborhood, after every collection was read.
func scanRecordsComplete(records map[string]string) error {
	for key, kind := range records {
		if kind != "GeneratedNetworkPolicy" {
			continue
		}
		name := key[len("generatednetworkpolicies/"):]
		if records["networkneighborhoods/"+name] != "NetworkNeighborhood" {
			return errors.New("a computed scan record has no stored source")
		}
	}
	return nil
}
