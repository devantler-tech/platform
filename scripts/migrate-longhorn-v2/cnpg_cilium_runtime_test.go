package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func runtimeCiliumFixture(t *testing.T) (object, object, []byte) {
	t.Helper()
	h, err := os.ReadFile("testdata/cilium-hive-synthetic-1.20.5.txt")
	if err != nil {
		t.Fatal(err)
	}
	c := object{"ClusterName": "default", "EnableNonDefaultDenyPolicies": true, "EnableL7Proxy": true, "RestoreState": true, "policy-enforcement": "default"}
	v := object{"Client": object{"Version": "1.20.5", "Revision": "15012141"}, "Daemon": object{"Version": "1.20.5", "Revision": "15012141"}}
	return c, v, h
}

func encodeRuntimeFixture(t *testing.T, o object) []byte {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRecoveryReadsNativeRuntimeConfigAndHiveCell(t *testing.T) {
	c, v, h := runtimeCiliumFixture(t)
	cfg, err := readRecoveryCiliumRuntime(encodeRuntimeFixture(t, c), encodeRuntimeFixture(t, v), h)
	if err != nil || cfg != reviewedRecoveryCiliumConfig() {
		t.Fatalf("native runtime configuration: %+v %v", cfg, err)
	}
	// Other object descriptions and function signatures may mention the type.
	h = append([]byte("provide: func(types.PolicyConfig)\n"), h...)
	if _, err = readRecoveryCiliumRuntime(encodeRuntimeFixture(t, c), encodeRuntimeFixture(t, v), h); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRefusesMissingOrChangedAgentRuntimeEvidence(t *testing.T) {
	for name, change := range map[string]func(object, object, []byte) []byte{
		"daemon unavailable with successful JSON": func(_, v object, h []byte) []byte { v["Daemon"] = object{}; return h },
		"daemon changed":          func(_, v object, h []byte) []byte { v["Daemon"].(object)["Version"] = "1.20.4"; return h },
		"unreviewed daemon build": func(_, v object, h []byte) []byte { v["Daemon"].(object)["Revision"] = "aaaaaaaa"; return h },
		"different client build":  func(_, v object, h []byte) []byte { v["Client"].(object)["Revision"] = "150121410"; return h },
		"unbound short revision": func(_, v object, h []byte) []byte {
			v["Client"].(object)["Revision"] = "150"
			v["Daemon"].(object)["Revision"] = "150"
			return h
		},
		"different cluster":        func(c, _ object, h []byte) []byte { c["ClusterName"] = "other"; return h },
		"missing non-default deny": func(c, _ object, h []byte) []byte { delete(c, "EnableNonDefaultDenyPolicies"); return h },
		"L7 disabled":              func(c, _ object, h []byte) []byte { c["EnableL7Proxy"] = false; return h },
		"quoted boolean":           func(c, _ object, h []byte) []byte { c["EnableL7Proxy"] = "true"; return h },
		"missing restore state":    func(c, _ object, h []byte) []byte { delete(c, "RestoreState"); return h },
		"restore state disabled":   func(c, _ object, h []byte) []byte { c["RestoreState"] = false; return h },
		"quoted restore state":     func(c, _ object, h []byte) []byte { c["RestoreState"] = "true"; return h },
		"no policy enforcement":    func(c, _ object, h []byte) []byte { c["policy-enforcement"] = "never"; return h },
		"missing live Hive cell":   func(_, _ object, _ []byte) []byte { return nil },
		"duplicate live Hive cell": func(_, _ object, h []byte) []byte { return append(h, h...) },
		"false local cluster cell": func(_, _ object, h []byte) []byte {
			return []byte(strings.ReplaceAll(string(h), "PolicyDefaultLocalCluster: (bool) true", "PolicyDefaultLocalCluster: (bool) false"))
		},
		"unknown cell field": func(_, _ object, h []byte) []byte {
			return []byte(strings.ReplaceAll(string(h), "PolicyDefaultLocalCluster:", "ChangedPolicyDefaultLocalCluster:"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, v, h := runtimeCiliumFixture(t)
			h = change(c, v, h)
			if _, err := readRecoveryCiliumRuntime(encodeRuntimeFixture(t, c), encodeRuntimeFixture(t, v), h); err == nil {
				t.Fatal("unbound runtime evidence accepted")
			}
		})
	}
}
