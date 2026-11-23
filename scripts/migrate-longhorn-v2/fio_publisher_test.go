package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFioPublisherAcceptsThePreviouslyTestedImageOnRerun(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/publish-longhorn-fio.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Env  map[string]string
		Jobs map[string]struct {
			Env, Outputs map[string]string
			Steps        []struct{ Name, Run string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	verify, publish := workflow.Jobs["verify"], workflow.Jobs["publish"]
	if verify.Outputs["version"] != "${{ steps.save.outputs.version }}" ||
		publish.Env["VERSION"] != "${{ needs.verify.outputs.version }}" {
		t.Fatal("publishing must use the version from the attempt whose image was tested")
	}
	var recheck string
	for _, step := range publish.Steps {
		if step.Name == "Recheck the tested image" {
			recheck = step.Run
		}
	}
	if recheck == "" {
		t.Fatal("publish must verify its loaded image")
	}
	dir := t.TempDir()
	testedImage := []byte("tested image")
	if err := os.WriteFile(filepath.Join(dir, "longhorn-fio.tar"), testedImage, 0600); err != nil {
		t.Fatal(err)
	}
	mockDocker := `#!/bin/bash
set -eu
case "$1 $2" in
  "load -i") test "$3" = "$RUNNER_TEMP/longhorn-fio.tar" ;;
  "image inspect")
    test "$5" = "${IMAGE}:3.41-${GITHUB_SHA}-1"
    printf '%s\n' "$EXPECTED_ID"
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(mockDocker), 0700); err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("1", 40)
	for _, tc := range []struct {
		name, version string
		wantSuccess   bool
	}{
		{"same source earlier build", "3.41-" + revision + "-1", true},
		{"recomputed publish attempt", "3.41-" + revision + "-2", false},
		{"other source", "3.41-" + strings.Repeat("2", 40) + "-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-c", recheck)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"RUNNER_TEMP="+dir, "IMAGE="+workflow.Env["IMAGE"], "GITHUB_SHA="+revision,
				"GITHUB_RUN_ATTEMPT=2", "VERSION="+tc.version,
				fmt.Sprintf("EXPECTED_SHA=%x", sha256.Sum256(testedImage)),
				"EXPECTED_ID=sha256:"+strings.Repeat("3", 64))
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("artifact recheck: %v\n%s", err, out)
			}
		})
	}
}

func TestFioPublisherSelectsTheTestedLinuxAMD64Manifest(t *testing.T) {
	index := "sha256:" + strings.Repeat("1", 64)
	image := "sha256:" + strings.Repeat("2", 64)
	attestation := "sha256:" + strings.Repeat("3", 64)
	config := "sha256:" + strings.Repeat("4", 64)
	imageDescriptor := fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"platform":{"os":"linux","architecture":"amd64"}}`, image)
	attestationDescriptor := fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"platform":{"os":"unknown","architecture":"unknown"}}`, attestation)
	imageManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q}}`, config)
	imageIndex := func(descriptors ...string) string {
		return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[%s]}`, strings.Join(descriptors, ","))
	}
	for _, tc := range []struct {
		name, published, tested, manifest, want string
	}{
		{"OCI index with BuildKit attestation", index, index, imageIndex(imageDescriptor, attestationDescriptor), image},
		{"direct image with config identity", image, config, imageManifest, image},
		{"direct image with manifest identity", image, image, imageManifest, image},
		{"wrong index identity", index, config, imageIndex(imageDescriptor, attestationDescriptor), ""},
		{"wrong config identity", image, attestation, imageManifest, ""},
		{"ambiguous platform", index, index, imageIndex(imageDescriptor, imageDescriptor), ""},
		{"missing platform", index, index, imageIndex(attestationDescriptor), ""},
		{"invalid child digest", index, index, imageIndex(strings.Replace(imageDescriptor, image, "invalid", 1)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "jq", "-er", "--arg", "published", tc.published, "--arg", "tested", tc.tested,
				"-f", "fio/published-amd64-digest.jq")
			cmd.Stdin = strings.NewReader(tc.manifest)
			out, err := cmd.CombinedOutput()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unsafe image identity was accepted: %s", out)
				}
				return
			}
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("wrong manifest: %v\n%s", err, out)
			}
		})
	}
}
