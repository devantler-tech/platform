package main

// helmChartLayerMediaType is the layer a Flux OCIRepository selects when the
// artifact is a Helm chart; a manifests artifact selects no such layer.
const helmChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// chartSourceTagSurfaceDocument represents the exact SemVer tag of a Helm chart
// OCIRepository by the same sentinel an exact HelmRelease chart version gets, so
// a reviewed dependency update that moves only that tag keeps its approval
// (#4788). The reference must be the tag alone: a digest, a SemVer range, a
// substituted or floating tag, and every non-chart source stay fingerprinted as
// written, as do the URL, the layer selector and the signature requirement.
func chartSourceTagSurfaceDocument(identity resourceIdentity, document map[string]any) map[string]any {
	if identity.apiVersion != "source.toolkit.fluxcd.io/v1" || identity.kind != "OCIRepository" {
		return document
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return document
	}
	layerSelector, ok := spec["layerSelector"].(map[string]any)
	if !ok || layerSelector["mediaType"] != helmChartLayerMediaType {
		return document
	}
	ref, ok := spec["ref"].(map[string]any)
	if !ok || len(ref) != 1 {
		return document
	}
	tag, ok := ref["tag"].(string)
	if !ok || !exactPinnedHelmChartVersion.MatchString(tag) {
		return document
	}

	projected := cloneStringAnyMap(document)
	projectedSpec := cloneStringAnyMap(spec)
	projectedSpec["ref"] = map[string]any{"tag": "<exact-semver>"}
	projected["spec"] = projectedSpec
	return projected
}
