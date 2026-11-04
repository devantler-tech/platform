package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGrypeNativeConfigCannotBroadenOrSilenceTheFullScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".grype.yaml")
	args := []string{"--config", path, "--exclude", "**/.agents", "--exclude", "**/megalinter-reports", "dir:."}
	for _, candidate := range []struct {
		data string
		want bool
	}{
		{"fail-on-severity: high\nignore:\n  - vulnerability: CVE-2018-20225\n", true},
		{"fail-on-severity: critical\nignore:\n  - vulnerability: CVE-2018-20225\n", false},
		{"fail-on-severity: high\nexclude: ['**/go.mod']\nignore:\n  - vulnerability: CVE-2018-20225\n", false},
		{"fail-on-severity: high\nignore:\n  - vulnerability: GO-other\n", false},
	} {
		if err := os.WriteFile(path, []byte(candidate.data), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := configuration(args)
		if (err == nil) != candidate.want {
			t.Fatalf("wrong config boundary: %v", err)
		}
	}
	for _, changed := range [][]string{{"dir:scripts"}, {"dir:.", "--only-fixed"}, {"dir:.", "--fail-on", "critical"}} {
		if _, _, err := configuration(changed); err == nil {
			t.Fatal("accepted changed native scope")
		}
	}
}
