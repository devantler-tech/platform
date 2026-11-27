package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func runtimeCiliumFixture() (object, object) {
	c := object{"ClusterName": "default", "EnableNonDefaultDenyPolicies": true, "EnableL7Proxy": true, "RestoreState": true, "policy-enforcement": "default"}
	v := object{"Client": object{"Version": "1.20.5", "Revision": "15012141"}, "Daemon": object{"Version": "1.20.5", "Revision": "15012141"}}
	return c, v
}

func encodeRuntimeFixture(t *testing.T, o object) []byte {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecoveryBindsNativeRuntimeToImportedPolicy(t *testing.T) {
	c, v := runtimeCiliumFixture()
	policy, entries := isolationFixture(t, "umami-db")
	endpoint := "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com"
	config, version := encodeRuntimeFixture(t, c), encodeRuntimeFixture(t, v)
	cfg, revision, err := readRecoveryCiliumRuntime(config, version, isolationResponse(t, entries, 817), policy, endpoint)
	if err != nil || cfg != reviewedRecoveryCiliumConfig() || revision != 817 {
		t.Fatalf("native runtime configuration: %+v at revision %d: %v", cfg, revision, err)
	}
	// Cilium's parser omits these peer constraints under all-clusters normalization.
	for _, entry := range entries {
		peers, _ := entry["L3"].([]any)
		for _, peer := range peers {
			if selector, ok := peer.(object); ok {
				labels, _ := selector["matchLabels"].(object)
				delete(labels, "k8s:io.cilium.k8s.policy.cluster")
			}
		}
	}
	for _, raw := range [][]byte{nil, isolationResponse(t, entries, 817)} {
		cfg, revision, err = readRecoveryCiliumRuntime(config, version, raw, policy, endpoint)
		if err == nil || cfg != (recoveryCiliumConfig{}) || revision != 0 {
			t.Fatal("missing or nonlocal policy import qualified the runtime")
		}
	}
}

func TestRecoveryRefusesMissingOrChangedAgentRuntimeEvidence(t *testing.T) {
	policy, entries := isolationFixture(t, "umami-db")
	repository := isolationResponse(t, entries, 817)
	endpoint := "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com"
	for name, change := range map[string]func(object, object){
		"daemon unavailable with successful JSON": func(_, v object) { v["Daemon"] = object{} },
		"daemon changed":          func(_, v object) { v["Daemon"].(object)["Version"] = "1.20.4" },
		"unreviewed daemon build": func(_, v object) { v["Daemon"].(object)["Revision"] = "aaaaaaaa" },
		"different client build":  func(_, v object) { v["Client"].(object)["Revision"] = "150121410" },
		"unbound short revision": func(_, v object) {
			v["Client"].(object)["Revision"] = "150"
			v["Daemon"].(object)["Revision"] = "150"
		},
		"different cluster":        func(c, _ object) { c["ClusterName"] = "other" },
		"missing non-default deny": func(c, _ object) { delete(c, "EnableNonDefaultDenyPolicies") },
		"L7 disabled":              func(c, _ object) { c["EnableL7Proxy"] = false },
		"quoted boolean":           func(c, _ object) { c["EnableL7Proxy"] = "true" },
		"missing restore state":    func(c, _ object) { delete(c, "RestoreState") },
		"restore state disabled":   func(c, _ object) { c["RestoreState"] = false },
		"quoted restore state":     func(c, _ object) { c["RestoreState"] = "true" },
		"no policy enforcement":    func(c, _ object) { c["policy-enforcement"] = "never" },
	} {
		t.Run(name, func(t *testing.T) {
			c, v := runtimeCiliumFixture()
			change(c, v)
			if _, _, err := readRecoveryCiliumRuntime(encodeRuntimeFixture(t, c), encodeRuntimeFixture(t, v), repository, policy, endpoint); err == nil {
				t.Fatal("unbound runtime evidence accepted")
			}
		})
	}
}
