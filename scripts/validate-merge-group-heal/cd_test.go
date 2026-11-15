package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validCDWorkflow = `jobs:
  deploy-prod:
    steps:
      - name: Deploy production
        uses: ./.github/actions/deploy-prod
        with:
          sops-age-key: placeholder
`

func TestCDDeployContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, workflow string
		wantCode       int
	}{
		{"CD without heal or membership jobs", validCDWorkflow, 0},
		{"wrong composite", strings.Replace(validCDWorkflow, deployCompositePath, "./.github/actions/other", 1), 1},
		{"missing deploy job", strings.Replace(validCDWorkflow, "  deploy-prod:", "  other:", 1), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cd.yaml")
			if err := os.WriteFile(path, []byte(tc.workflow), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			code := runCLI([]string{"--deploy-only", path}, &out, &errOut)
			if code != tc.wantCode {
				t.Fatalf("code=%d want=%d stderr=%s", code, tc.wantCode, &errOut)
			}
			if tc.wantCode != 0 && (!strings.Contains(errOut.String(), path) || out.Len() != 0) {
				t.Fatalf("failed CD check must name its input and never report success: out=%q err=%q", &out, &errOut)
			}
		})
	}
}

func TestActualCDDeployRemovalFails(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/cd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(workflow), "uses: "+deployCompositePath+"\n") != 1 {
		t.Fatal("expected one actual CD deploy composite call")
	}
	for _, removed := range []bool{false, true} {
		input := string(workflow)
		if removed {
			input = strings.Replace(input, "uses: "+deployCompositePath+"\n", "uses: ./.github/actions/other\n", 1)
		}
		path := filepath.Join(t.TempDir(), "cd.yaml")
		if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		code := runCLI([]string{"--deploy-only", path}, &out, &errOut)
		if !removed && code != 0 {
			t.Fatalf("actual CD rejected: %s", &errOut)
		}
		if removed && (code != 1 || !strings.Contains(errOut.String(), "cd.yaml") || !strings.Contains(errOut.String(), "does not reach the shared deploy composite")) {
			t.Fatalf("actual removal not caught: code=%d err=%s", code, &errOut)
		}
	}
}

func TestCDDeployCLIReadAndArgumentErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--deploy-only"}, {"--deploy-only", "cd.yaml", "extra"}, {"--wrong", "cd.yaml"}} {
		var out, errOut bytes.Buffer
		if code := runCLI(args, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Fatalf("args=%v code=%d out=%q", args, code, &out)
		}
	}
	path := filepath.Join(t.TempDir(), "missing-cd.yaml")
	var out, errOut bytes.Buffer
	if code := runCLI([]string{"--deploy-only", path}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), path) || out.Len() != 0 {
		t.Fatalf("missing file code=%d out=%q err=%q", code, &out, &errOut)
	}
}
