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
