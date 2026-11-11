package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKubectlDownloadCurlChild(_ *testing.T) {
	if os.Getenv("ALERT_DOWNLOAD_CHILD") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	want := []string{"--disable", "--fail", "--silent", "--show-error", "--location", "--proto", "=https", "--proto-redir", "=https", "--max-redirs", "3", "--connect-timeout", "10", "--max-time", "60", "--retry", "3", "--retry-all-errors", "--retry-delay", "5", "--retry-max-time", "120", "https://dl.k8s.io/release/v1.36.2/bin/linux/amd64/kubectl", "-o", os.Getenv("RUNNER_TEMP") + "/alert-routes-kubectl"}
	if !reflect.DeepEqual(args, want) {
		os.Exit(31)
	}
	os.Exit(0)
}

// Execute the real rendered curl statement. A second positional URL (the
// regression) must fail before checksum/install can use a missing download.
func TestWorkflowDownloadPassesOnlyOneURLToCurl(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/read-alert-routes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Run string } }
	}
	if yaml.Unmarshal(raw, &workflow) != nil {
		t.Fatal("workflow decode")
	}
	statement := ""
	for _, step := range workflow.Jobs["observe"].Steps {
		lines := strings.Split(step.Run, "\n")
		for i, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "curl ") {
				continue
			}
			statement = line
			for strings.HasSuffix(strings.TrimSpace(lines[i]), "\\") {
				i++
				if i >= len(lines) {
					t.Fatal("unfinished download")
				}
				statement += "\n" + lines[i]
			}
		}
	}
	if statement == "" {
		t.Fatal("download missing")
	}
	bin := t.TempDir()
	testbin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(testbin, "'", "'\\''") + "' -test.run='^TestKubectlDownloadCurlChild$' -- \"$@\"\n"
	if os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0700) != nil {
		t.Fatal("fixture unavailable")
	}
	command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", "set -euo pipefail\nkubectl_path=\"$RUNNER_TEMP/alert-routes-kubectl\"\n"+statement+"\nprintf DOWNLOAD_ADMITTED")
	command.Env = []string{"ALERT_DOWNLOAD_CHILD=1", "PATH=" + bin, "RUNNER_TEMP=" + bin, "KUBECTL_VERSION=v1.36.2"}
	// A coverage run exports GOCOVERDIR. Without it the instrumented child
	// warns on stderr at exit, which this exact-output check would read as
	// leaked output; forwarding it keeps the check exact and records coverage.
	if dir, ok := os.LookupEnv("GOCOVERDIR"); ok {
		command.Env = append(command.Env, "GOCOVERDIR="+dir)
	}
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Equal(output, []byte("DOWNLOAD_ADMITTED")) {
		t.Fatalf("download command rejected: %v %q", err, output)
	}
}
