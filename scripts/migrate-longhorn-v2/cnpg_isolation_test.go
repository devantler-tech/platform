package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func reviewedRecoveryCiliumConfig() recoveryCiliumConfig {
	return recoveryCiliumConfig{Version: "1.20.5", Cluster: "default", Enforcement: "default", DefaultLocalCluster: true, NonDefaultDeny: true, L7: true, RestoreState: true}
}

func isolationFixture(t *testing.T, cluster string) (object, []object) {
	t.Helper()
	c, s, b := recoverySourceFixture(cluster)
	source, err := bindCNPGRecoverySource(c, s, b, text(b, "metadata", "uid"), archiveBindingFixture(c, s))
	if err != nil {
		t.Fatal(err)
	}
	resources, err := cnpgRecoveryResources("1234", source, "ghcr.io/cloudnative-pg/postgresql@sha256:"+strings.Repeat("a", 64), postgresState{SystemID: "1234567890123456789", Timeline: "7", FlushLSN: "2/100"}, "qualified-worker")
	if err != nil {
		t.Fatal(err)
	}
	p := resources[1]
	p["metadata"].(object)["uid"] = "11111111-1111-4111-8111-111111111111"
	p["metadata"].(object)["generation"] = float64(1)
	raw, err := os.ReadFile("testdata/cnpg-isolation-entries-cilium-1.20.5.json")
	if err != nil {
		t.Fatal(err)
	}
	// The native fixture was generated for Umami; this replacement changes only
	// the recovery namespace, preserving the production namespace denies.
	raw = []byte(strings.ReplaceAll(string(raw), "longhorn-db-proof-umami-db-1234", text(p, "metadata", "namespace")))
	var entries []object
	if err = json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	return p, entries
}

func isolationResponse(t *testing.T, entries []object, revision int) []byte {
	t.Helper()
	inner, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(object{"revision": revision, "policy": string(inner)})
	if err != nil {
		t.Fatal(err)
	}
	return outer
}

func TestRecoveryIsolationUsesNativeFlattenedCiliumPolicy(t *testing.T) {
	for _, source := range cnpgArchiveSources() {
		p, entries := isolationFixture(t, source.Name)
		// Native repository iteration is unordered. An unrelated policy does not
		// become part of this CNP's exact imported multiset.
		entries[0], entries[6] = entries[6], entries[0]
		_, unrelated := isolationFixture(t, source.Name)
		unrelated[0]["Labels"] = nil
		entries = append(entries, unrelated[0])
		revision, err := verifyCNPGIsolationImport(isolationResponse(t, entries, 817), p, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig())
		if err != nil || revision != 817 {
			t.Fatalf("native policy %s: %d %v", source.Name, revision, err)
		}
	}
}

func TestRecoveryIsolationRejectsUnrelatedInitializationAllowance(t *testing.T) {
	policy, entries := isolationFixture(t, "umami-db")
	entries = append(entries, object{"Labels": nil, "Node": false, "Ingress": false, "Verdict": float64(0), "Subject": object{}})
	if _, err := verifyCNPGIsolationImport(isolationResponse(t, entries, 817), policy, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig()); err == nil {
		t.Fatal("exact CNP import hid an unrelated initialization allowance")
	}
}

func TestRecoveryIsolationRefusesIncompleteOrWidenedNativeRules(t *testing.T) {
	for name, change := range map[string]func([]object) []object{
		"missing production deny": func(e []object) []object { return e[:6] },
		"duplicate imported rule": func(e []object) []object { return append(e, e[0]) },
		"missing default deny":    func(e []object) []object { e[0]["DefaultDeny"] = false; return e },
		"wrong namespace": func(e []object) []object {
			value(e[0], "Subject", "matchLabels").(object)["k8s:io.kubernetes.pod.namespace"] = "unrelated"
			return e
		},
		"wrong local cluster": func(e []object) []object {
			value(value(e[0], "L3").([]any)[0].(object), "matchLabels").(object)["k8s:io.cilium.k8s.policy.cluster"] = "other"
			return e
		},
		"widened operator ingress": func(e []object) []object { e[0]["L3"] = []any{object{}}; return e },
		"missing DNS restriction":  func(e []object) []object { value(e[2], "L4").([]any)[0].(object)["rules"] = nil; return e },
		"different archive":        func(e []object) []object { e[3]["L3"] = []any{"MatchName: other.invalid, MatchPattern: "}; return e },
		"deny became allow":        func(e []object) []object { e[4]["Verdict"] = float64(0); return e },
		"unexpected priority":      func(e []object) []object { e[0]["Priority"] = float64(1); return e },
		"old policy UID":           func(e []object) []object { value(e[0], "Labels").([]any)[3].(object)["value"] = "old"; return e },
		"duplicate UID label": func(e []object) []object {
			labels := value(e[0], "Labels").([]any)
			e[0]["Labels"] = append(labels, labels[3])
			return e
		},
		"missing UID label": func(e []object) []object { e[0]["Labels"] = value(e[0], "Labels").([]any)[:3]; return e },
	} {
		t.Run(name, func(t *testing.T) {
			p, e := isolationFixture(t, "umami-db")
			if _, err := verifyCNPGIsolationImport(isolationResponse(t, change(e), 817), p, "https://"+strings.Repeat("a", 32)+".r2.cloudflarestorage.com", reviewedRecoveryCiliumConfig()); err == nil {
				t.Fatal("incomplete or widened isolation accepted")
			}
		})
	}
}

func TestRecoveryIsolationRefusesUnboundOrMalformedObservations(t *testing.T) {
	p, e := isolationFixture(t, "umami-db")
	endpoint := "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com"
	for _, raw := range [][]byte{nil, []byte(`{"revision":0,"policy":"[]"}`), []byte(`{"revision":1,"policy":[]}`), []byte(`{"revision":1,"policy":"[]","unexpected":true}`), append(isolationResponse(t, e, 817), []byte(` {}`)...)} {
		if _, err := verifyCNPGIsolationImport(raw, p, endpoint, reviewedRecoveryCiliumConfig()); err == nil {
			t.Fatal("malformed observation accepted")
		}
	}
	for _, change := range []func(*recoveryCiliumConfig){func(c *recoveryCiliumConfig) { c.Version = "1.20.3" }, func(c *recoveryCiliumConfig) { c.Cluster = "other" }, func(c *recoveryCiliumConfig) { c.DefaultLocalCluster = false }, func(c *recoveryCiliumConfig) { c.NonDefaultDeny = false }, func(c *recoveryCiliumConfig) { c.L7 = false }, func(c *recoveryCiliumConfig) { c.Enforcement = "never" }} {
		cfg := reviewedRecoveryCiliumConfig()
		change(&cfg)
		if _, err := verifyCNPGIsolationImport(isolationResponse(t, e, 817), p, endpoint, cfg); err == nil {
			t.Fatal("unreviewed agent configuration accepted")
		}
	}
	p["spec"].(object)["egressDeny"] = []any{}
	if _, err := verifyCNPGIsolationImport(isolationResponse(t, e, 817), p, endpoint, reviewedRecoveryCiliumConfig()); err == nil {
		t.Fatal("changed recovery CNP accepted")
	}
}
