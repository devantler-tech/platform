package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestStorageWorkflowBindsTheSelectedOperation runs both the precredential guard and real command selection.
func TestStorageWorkflowBindsTheSelectedOperation(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/protect-backstage-storage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w storageWorkflow
	if yaml.Unmarshal(b, &w) != nil {
		t.Fatal("invalid workflow")
	}
	inputs := w.On["workflow_dispatch"].(map[string]any)["inputs"].(map[string]any)
	want := map[string]any{"description": "protect changes bound Delete policies to Retain; check reads only", "required": true, "type": "choice", "default": "protect", "options": []any{"protect", "check"}}
	if !reflect.DeepEqual(inputs["operation"], want) || w.Jobs["protect"].Env["PORTAL_STORAGE_OPERATION"] != "${{ inputs.operation }}" {
		t.Fatal("read-only mode must be an explicit, default-off workflow choice")
	}
	var dispatch string
	for _, step := range w.Jobs["protect"].Steps {
		if strings.Contains(step.Run, "go run ./scripts/protect-backstage-storage --execute") {
			dispatch = step.Run
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"protect", "check", "", "CHECK", "check;protect"} {
		for _, confirmation := range []string{"PROTECT BACKSTAGE STORAGE ONLY", "CHECK BACKSTAGE STORAGE ONLY"} {
			t.Run(operation+"/"+confirmation, func(t *testing.T) {
				env := testEnvironment()
				env["PORTAL_STORAGE_OPERATION"] = operation
				env["PORTAL_STORAGE_CONFIRM"] = confirmation
				valid := operation == "protect" && confirmation == "PROTECT BACKSTAGE STORAGE ONLY" || operation == "check" && confirmation == "CHECK BACKSTAGE STORAGE ONLY"
				cmd := exec.Command("bash", "-c", w.Jobs["protect"].Steps[0].Run)
				for key, value := range env {
					cmd.Env = append(cmd.Env, key+"="+value)
				}
				if output, err := cmd.CombinedOutput(); (err == nil) != valid {
					t.Fatalf("precredential operation guard mismatch: error=%v output=%s", err, output)
				}
				if !valid {
					return
				}
				cmd = exec.Command("bash", "-c", dispatch)
				cmd.Env = []string{"PATH=" + bin, "PORTAL_STORAGE_OPERATION=" + operation}
				flag := "--execute"
				if operation == "check" {
					flag = "--check"
				}
				if output, err := cmd.CombinedOutput(); err != nil || string(output) != "run ./scripts/protect-backstage-storage "+flag+"\n" {
					t.Fatalf("selected operation invoked the wrong command: error=%v output=%s", err, output)
				}
			})
		}
	}
}
