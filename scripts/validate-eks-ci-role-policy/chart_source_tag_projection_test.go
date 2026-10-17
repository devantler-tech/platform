package main

import (
	"strings"
	"testing"
)

// TestChartSourceTagProjectionAllowsOnlyExactTagMoves proves that moving the
// exact SemVer tag of a Helm chart source keeps its approval, and that nothing
// else about the source does.
func TestChartSourceTagProjectionAllowsOnlyExactTagMoves(t *testing.T) {
	const manifest = `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: operator
  namespace: operator
spec:
  interval: 10m
  url: oci://registry.example/charts/operator
  ref:
    tag: 1.2.3
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\.actions\.githubusercontent\.com$'
        subject: '^https://github\.com/example/operator/\.github/workflows/cd\.yaml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$'
`
	entry := func(t *testing.T, contents string) string {
		t.Helper()
		documents, err := decodeDocuments([]byte(contents))
		if err != nil || len(documents) != 1 {
			t.Fatalf("decode OCIRepository: documents=%d error=%v", len(documents), err)
		}
		before, err := canonicalFingerprint(documents[0])
		if err != nil {
			t.Fatal(err)
		}
		actual, err := authorizationSurfaceEntry(identityOf(documents[0]), documents[0])
		if err != nil {
			t.Fatal(err)
		}
		after, err := canonicalFingerprint(documents[0])
		if err != nil || after != before {
			t.Fatalf("projection mutated its input: before=%s after=%s error=%v", before, after, err)
		}
		return actual
	}
	replace := func(t *testing.T, old, replacement string) string {
		t.Helper()
		if !strings.Contains(manifest, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return strings.Replace(manifest, old, replacement, 1)
	}
	baseline := entry(t, manifest)

	for _, tag := range []string{"1.2.4", "7.202.53", "v2.0.0", `"0.15.0"`, "1.0.0-rc.1"} {
		t.Run("exact tag "+tag, func(t *testing.T) {
			if actual := entry(t, replace(t, "tag: 1.2.3", "tag: "+tag)); actual != baseline {
				t.Fatalf("an exact tag move changed the entry:\n%s\n%s", baseline, actual)
			}
		})
	}

	const digest = "sha256:bdfdad17cf8e45ae064c73ed6764f0c75d4bf71a6e2912e6363a43d6dfc787de"
	changes := map[string]string{
		"floating tag":         replace(t, "tag: 1.2.3", "tag: latest"),
		"partial version":      replace(t, "tag: 1.2.3", `tag: "1.2"`),
		"wildcard tag":         replace(t, "tag: 1.2.3", `tag: "1.2.x"`),
		"substituted tag":      replace(t, "tag: 1.2.3", "tag: ${operator_version}"),
		"semver range":         replace(t, "tag: 1.2.3", `semver: ">=1.0.0"`),
		"tag with digest":      replace(t, "tag: 1.2.3", "tag: 1.2.3\n    digest: "+digest),
		"digest only":          replace(t, "tag: 1.2.3", "digest: "+digest),
		"tag with semver":      replace(t, "tag: 1.2.3", "tag: 1.2.3\n    semver: \">=1.0.0\""),
		"another registry":     replace(t, "registry.example/charts/operator", "registry.example/charts/other"),
		"not a chart layer":    replace(t, "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "application/vnd.example.content"),
		"another layer action": replace(t, "operation: copy", "operation: extract"),
		"no signature":         manifest[:strings.Index(manifest, "  verify:")],
		"another signer":       replace(t, "example/operator/", "someone-else/operator/"),
		"another interval":     replace(t, "interval: 10m", "interval: 1h"),
	}
	for name, contents := range changes {
		t.Run(name, func(t *testing.T) {
			if actual := entry(t, contents); actual == baseline {
				t.Fatalf("%s kept the approved entry", name)
			}
		})
	}

	// A source that is not a Helm chart keeps its tag in the fingerprint, and so
	// does one whose reference is not the tag alone.
	exact := map[string][2]string{
		"manifests source": {
			strings.Replace(manifest, "  layerSelector:\n    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip\n    operation: copy\n", "", 1),
			"tag: 1.2.3",
		},
		"tag with digest": {changes["tag with digest"], "tag: 1.2.3"},
	}
	for name, source := range exact {
		t.Run(name+" stays exact", func(t *testing.T) {
			if !strings.Contains(source[0], source[1]) {
				t.Fatalf("fixture does not contain %q", source[1])
			}
			moved := strings.Replace(source[0], source[1], "tag: 1.2.4", 1)
			if entry(t, source[0]) == entry(t, moved) {
				t.Fatalf("%s: a tag move kept the entry", name)
			}
		})
	}
}
