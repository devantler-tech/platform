package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestImmutableRuntimeDescriptorJoin(t *testing.T) {
	child := "sha256:" + strings.Repeat("a", 64)
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}},{"digest":"attestation","platform":{"os":"unknown","architecture":"unknown"}}]}`, child)
	digest := func(data string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(data))) }
	for _, suffix := range []string{"", "\n"} {
		got, err := runtimeDigest([]byte(index+suffix), digest(index))
		if err != nil || got != child {
			t.Fatalf("signed index join: %s %v", got, err)
		}
	}
	for name, data := range map[string]string{
		"tampered":           index + " ",
		"no-runtime":         strings.ReplaceAll(index, "amd64", "arm64"),
		"malformed-digest":   strings.ReplaceAll(index, child, "latest"),
		"unsupported-schema": strings.ReplaceAll(index, `"schemaVersion":2`, `"schemaVersion":1`),
		"unknown-media":      strings.ReplaceAll(index, "application/vnd.oci.image.index.v1+json", "unknown"),
		"trailing-json":      index + "{}",
		"ambiguous":          fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}},{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, child, child),
	} {
		t.Run(name, func(t *testing.T) {
			expected := digest(data)
			if name == "tampered" {
				expected = digest(index)
			}
			if _, err := runtimeDigest([]byte(data), expected); err == nil {
				t.Fatal("accepted unproven runtime descriptor")
			}
		})
	}
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`
	if got, err := runtimeDigest([]byte(manifest), digest(manifest)); err != nil || got != digest(manifest) {
		t.Fatal(got, err)
	}
}
