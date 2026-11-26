package recovery_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestV2ArchiveCopyPreservesOpenBaoSnapshots(t *testing.T) {
	const name = "openbao-20261126-033000.snap"
	objects := render(t, "k8s/providers/hetzner/infrastructure")
	job := object(t, objects, "Job", "copy-openbao-snapshots-to-v2")
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	placement, _ := pod["nodeSelector"].(map[string]any)
	if placement["platform.devantler.tech/baseline-worker"] != "true" {
		t.Fatal("the copy must mount its retained Hetzner volume on a baseline worker")
	}
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("copy must run without a Kubernetes credential")
	}
	volumes := pod["volumes"].([]any)
	original := volumes[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	destination := volumes[1].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if original["claimName"] != "vault-snapshots-recovery" || original["readOnly"] != true ||
		destination["claimName"] != "vault-snapshots-recovery-v2" || destination["readOnly"] == true {
		t.Fatal("copy must read the current provider disk and write only the prepared V2 claim")
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	script := container["args"].([]any)[0].(string)
	if strings.Contains(script, "${") {
		t.Fatal("copy must survive strict Flux substitution")
	}
	for _, tc := range []struct {
		name, sourceFile, destinationData string
		staging                           string
		success                           bool
	}{
		{name: "first copy", sourceFile: name, success: true},
		{name: "retry identical retained bytes", sourceFile: name, destinationData: "verified snapshot", success: true},
		{name: "refuse unrecognized data", sourceFile: "unexpected.bin"},
		{name: "refuse newline in a snapshot filename", sourceFile: name + "\n"},
		{name: "refuse malformed source with an identical destination", sourceFile: name + "\n", destinationData: "verified snapshot"},
		{name: "resume an owned interrupted staging file", sourceFile: name, staging: "owned", success: true},
		{name: "refuse unowned staging files", sourceFile: name, staging: "unknown"},
		{name: "refuse empty source"},
		{name: "refuse replacing different destination data", sourceFile: name, destinationData: "unexpected destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source, target, scratch := filepath.Join(root, "source"), filepath.Join(root, "target"), filepath.Join(root, "scratch")
			for _, dir := range []string{source, target, scratch} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, contents string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sourceFile != "" {
				write(filepath.Join(source, tc.sourceFile), "verified snapshot")
			}
			if tc.destinationData != "" {
				write(filepath.Join(target, name), tc.destinationData)
			}
			stage := filepath.Join(target, ".openbao-v2-copy")
			if tc.staging != "" {
				if err := os.Mkdir(stage, 0o700); err != nil {
					t.Fatal(err)
				}
				if tc.staging == "owned" {
					write(filepath.Join(stage, "openbao-v2-archive-copy"), "")
				}
				write(filepath.Join(stage, "snapshot"), "interrupted copy")
			}
			command := strings.NewReplacer("/original", source, "/replacement", target, "/scratch", scratch,
				"chgrp 1000", "chgrp "+strconv.Itoa(os.Getegid())).Replace(script)
			output, err := exec.CommandContext(t.Context(), "/bin/sh", "-ec", command).CombinedOutput()
			if (err == nil) != tc.success {
				t.Fatalf("copy success=%t, expected %t: %s", err == nil, tc.success, output)
			}
			if tc.success && !strings.Contains(string(output), "OPENBAO_V2_ARCHIVE_COPY=VERIFIED count=1") {
				t.Fatalf("missing complete copy verdict: %s", output)
			}
			if tc.staging == "unknown" {
				content, err := os.ReadFile(filepath.Join(stage, "snapshot"))
				if err != nil || string(content) != "interrupted copy" {
					t.Fatal("copy changed an unowned staging file")
				}
			}
			if tc.success {
				if _, err := os.Lstat(stage); !os.IsNotExist(err) {
					t.Fatal("the successful copy left staging files in the destination")
				}
			}
			want := tc.destinationData
			if tc.success {
				want = "verified snapshot"
			}
			result, readErr := os.ReadFile(filepath.Join(target, name))
			if want != "" && (readErr != nil || string(result) != want) {
				t.Fatal("copy overwrote independently existing data")
			}
			if tc.sourceFile != "" {
				result, err = os.ReadFile(filepath.Join(source, tc.sourceFile))
				if err != nil || string(result) != "verified snapshot" {
					t.Fatal("copy changed the source archive")
				}
			}
		})
	}
}
