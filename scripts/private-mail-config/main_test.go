package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandWritesOnlyAnExclusivePrivatePlan(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "inputs")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"listener-inventory.json": string(emptyInventory(t)),
		"settings.json":           `{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator"}`,
		"administrator-password":  strings.Repeat("A", 40), "maintainer-password": strings.Repeat("B", 40), "agent-password": strings.Repeat("C", 40),
	} {
		if err := os.WriteFile(filepath.Join(input, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "plan.ndjson")
	var log bytes.Buffer
	args := []string{"--input-dir", input, "--output-file", output}
	if command(args, &log) != 0 {
		t.Fatalf("configuration failed: %s", &log)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("plan is not private")
	}
	if strings.Contains(log.String(), strings.Repeat("A", 40)) || strings.Contains(log.String(), "example.test") {
		t.Fatal("private input appeared in output")
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if command(args, &log) == 0 {
		t.Fatal("existing plan silently replaced")
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing output changed")
	}
	if err = os.Remove(output); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(input, "settings.json"), []byte(`{"domain":"private-canary.invalid","injected":"secret-canary"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if command(args, &log) == 0 {
		t.Fatal("unknown settings accepted")
	}
	if _, err = os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("invalid input left an output plan")
	}
	if strings.Contains(log.String(), "canary") {
		t.Fatal("invalid private input echoed")
	}
}
