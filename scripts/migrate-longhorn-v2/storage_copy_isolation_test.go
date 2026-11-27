package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStorageCopyIsolationRequiresBothExactNativeDenyEntries(t *testing.T) {
	const run = "761"
	scope := corootKeeperCopyIsolation
	policy := corootKeeperCopyNetworkPolicy(run, "acef1536f21e1b8cf32d62c6045fc989fa8e55d6")
	uid := "fa865009-65e9-4b38-aa3d-08761e62ea39"
	name := scope.Prefix + run
	valueObject(policy, "metadata")["uid"] = uid
	exact := strings.NewReplacer(
		"storage-copy-isolation-1234", name, "storage-copy-fixture", corootKeeperNamespace,
		"11111111-1111-4111-8111-111111111111", uid, "1234", run,
		"storage.platform.devantler.tech/copy-run-fixture", corootKeeperCopyLabel,
	).Replace(string(storageCopyNativeDenyEntries))
	envelope := func(entries string) []byte {
		b, err := json.Marshal(object{"revision": 45, "policy": entries})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if revision, err := scope.verifyNativeDeny(envelope(exact), policy, run); err != nil || revision != 45 {
		t.Fatalf("the pinned Cilium evidence was rejected: %v", err)
	}
	var entries []object
	if err := json.Unmarshal([]byte(exact), &entries); err != nil {
		t.Fatal(err)
	}
	oneEntry, err := json.Marshal(entries[:1])
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{string(oneEntry), strings.Replace(exact, corootKeeperCopyLabel, "example.com/other-scope", 1),
		strings.Replace(exact, "observability", "default", 1)} {
		if _, err := scope.verifyNativeDeny(envelope(changed), policy, run); err == nil {
			t.Fatal("incomplete or different traffic denial was accepted")
		}
	}
}
