package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestRecoveryIsolationAccountsForTheReviewedGeneratedBaseline uses released compiler output.
func TestRecoveryIsolationAccountsForTheReviewedGeneratedBaseline(t *testing.T) {
	policy, entries := isolationFixture(t, "umami-db")
	raw, err := os.ReadFile("testdata/cnpg-baseline-entries-cilium-1.20.5.json")
	if err != nil {
		t.Fatal(err)
	}
	var baseline []object
	if json.Unmarshal(raw, &baseline) != nil || len(baseline) != 3 {
		t.Fatal("invalid generated baseline")
	}
	entries = append(entries, baseline...)
	if _, err = verifyCNPGIsolationImport(isolationResponse(t, entries, 817), policy, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig()); err != nil {
		t.Fatalf("reviewed generated baseline refused: %v", err)
	}
}

// TestRecoveryIsolationCannotWidenTheGeneratedResolverAllowance guards the baseline's destination and port bounds.
func TestRecoveryIsolationCannotWidenTheGeneratedResolverAllowance(t *testing.T) {
	for name, mutate := range map[string]func(object){
		"another port": func(o object) { value(value(o, "L4").([]any)[0].(object), "ports").([]any)[0].(object)["port"] = "443" },
		"all peers":    func(o object) { o["L3"] = []any{object{}} },
	} {
		t.Run(name, func(t *testing.T) {
			policy, entries := isolationFixture(t, "umami-db")
			raw, err := os.ReadFile("testdata/cnpg-baseline-entries-cilium-1.20.5.json")
			if err != nil {
				t.Fatal(err)
			}
			var baseline []object
			if json.Unmarshal(raw, &baseline) != nil || len(baseline) != 3 {
				t.Fatal("invalid baseline")
			}
			mutate(baseline[2])
			entries = append(entries, baseline...)
			if _, err = verifyCNPGIsolationImport(isolationResponse(t, entries, 817), policy, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig()); err == nil {
				t.Fatal("generated resolver allowance widened")
			}
		})
	}
}

// TestRecoveryIsolationRejectsBroaderAllowancesFromAnotherPolicy checks the effective rule union.
func TestRecoveryIsolationRejectsBroaderAllowancesFromAnotherPolicy(t *testing.T) {
	for _, ingress := range []bool{false, true} {
		t.Run(map[bool]string{false: "egress", true: "ingress"}[ingress], func(t *testing.T) {
			policy, entries := isolationFixture(t, "umami-db")
			foreign, err := cloneRecoverySpec(entries[0])
			if err != nil {
				t.Fatal(err)
			}
			foreign["Labels"] = nil
			foreign["Ingress"] = ingress
			foreign["Subject"] = object{"matchExpressions": []any{object{"key": "k8s:io.kubernetes.pod.namespace", "operator": "Exists"}}}
			foreign["L3"], foreign["L4"] = []any{object{}}, []any{}
			entries = append(entries, foreign)
			if _, err = verifyCNPGIsolationImport(isolationResponse(t, entries, 817), policy, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig()); err == nil {
				t.Fatal("another policy widened the recovery namespace")
			}
		})
	}
}

// TestRecoveryIsolationKeepsProvablyUnrelatedNamespacesOutsideItsEffectiveUnion avoids global ownership claims.
func TestRecoveryIsolationKeepsProvablyUnrelatedNamespacesOutsideItsEffectiveUnion(t *testing.T) {
	policy, entries := isolationFixture(t, "umami-db")
	foreign, err := cloneRecoverySpec(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	foreign["Labels"] = nil
	foreign["Ingress"] = false
	foreign["Subject"] = object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "unrelated"}}
	foreign["L3"], foreign["L4"] = []any{object{}}, []any{}
	entries = append(entries, foreign)
	if _, err = verifyCNPGIsolationImport(isolationResponse(t, entries, 817), policy, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig()); err != nil {
		t.Fatalf("unrelated namespace blocked recovery: %v", err)
	}
}
