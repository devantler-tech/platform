package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retirementFenceFixture(t *testing.T) benchmarkRecord {
	t.Helper()
	record, _, _, _, _ := recoveryStorageFixture(t)
	record.NamespaceUID = "scratch-namespace"
	for kind, name := range map[string]string{"Namespace": record.Namespace, "Pod": "fio", "NetworkPolicy": "fio-default-deny"} {
		namespace := record.Namespace
		if kind == "Namespace" {
			namespace = ""
		}
		record.Resources[kind] = benchmarkResource{Name: name, Namespace: namespace, UID: "scratch-" + kind}
	}
	record.Resources["Namespace"] = benchmarkResource{Name: record.Namespace, UID: record.NamespaceUID}
	return record
}

func TestRetirementFenceRequiresOriginalUIDReceipts(t *testing.T) {
	for _, fault := range []string{"missing backend", "missing PV UID", "pending create", "foreign namespace", "injected name"} {
		t.Run(fault, func(t *testing.T) {
			record := retirementFenceFixture(t)
			switch fault {
			case "missing backend":
				delete(record.Resources, "LonghornVolume")
			case "missing PV UID":
				pv := record.Resources["PersistentVolume"]
				pv.UID = ""
				record.Resources["PersistentVolume"] = pv
			case "pending create":
				record.PendingCreate = "Pod"
			case "foreign namespace":
				record.Namespace = "foreign"
			case "injected name":
				volume := record.Resources["LonghornVolume"]
				volume.Name = `owned" || true || "`
				record.Resources["LonghornVolume"] = volume
			}
			if _, err := benchmarkRetirementFences(record); err == nil {
				t.Fatal("unproven receipts became retirement policies")
			}
		})
	}
}

func TestNativeRetirementFenceDecisions(t *testing.T) {
	byPhase := map[string][]object{}
	for _, phase := range []string{"v1", "v2"} {
		record := retirementFenceFixture(t)
		record.Phase = phase
		record.Namespace = "longhorn-benchmark-" + phase + "-" + record.Run
		for kind, identity := range record.Resources {
			if kind == "Namespace" {
				identity.Name = record.Namespace
			}
			if identity.Namespace != "" && identity.Namespace != "longhorn-system" {
				identity.Namespace = record.Namespace
			}
			if kind == "StorageClass" {
				identity.Name = "lh-benchmark-" + phase + "-" + record.Run
			}
			record.Resources[kind] = identity
		}
		policies, err := benchmarkRetirementFences(record)
		if err != nil {
			t.Fatal(err)
		}
		_, groups := retirementInstanceFixture(t)
		for kind, items := range groups {
			if kind == "volumeattachments" {
				continue
			}
			for _, item := range items {
				item["spec"].(object)["dataEngine"] = phase
			}
		}
		noStart, err := benchmarkRetirementInstanceFence(record, groups, "5678-"+strings.Repeat("a", 32))
		if err != nil {
			t.Fatal(err)
		}
		policies = append(policies, noStart)
		byPhase[phase] = policies
	}
	raw, err := json.Marshal(byPhase)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "retirement-fences.json")
	if err := os.WriteFile(fixture, raw, 0600); err != nil {
		t.Fatal(err)
	}
	evaluate := func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, "go", "test", "-v", "-mod=readonly", "-count=1", "-timeout=2m", ".")
		command.Dir = "testdata/retirement-fence-proof"
		command.Env = append(os.Environ(), "LONGHORN_RETIREMENT_FENCE_PATH="+fixture)
		return command.CombinedOutput()
	}
	output, err := evaluate()
	if err != nil {
		t.Fatalf("native retirement-fence proof failed: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "--- PASS: TestNativeOriginalReferenceFences") {
		t.Fatal("native proof did not report its completed decisions")
	}
	// A permissive expression must be caught by actual admission decisions,
	// rather than succeeding because the native proof never selected the rule.
	for _, policies := range byPhase {
		for _, policy := range policies {
			policy["spec"].(object)["validations"].([]any)[0].(object)["expression"] = "true"
		}
	}
	mutated, err := json.Marshal(byPhase)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, mutated, 0600); err != nil {
		t.Fatal(err)
	}
	output, err = evaluate()
	if err == nil || !strings.Contains(string(output), "allow=true want=false") {
		t.Fatalf("permissive mutation was not caught by an adverse native decision: %v: %s", err, output)
	}
}
