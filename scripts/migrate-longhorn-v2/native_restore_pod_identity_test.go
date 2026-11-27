package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeRestoreWriteRefusesAReplacedConsumerBeforeTouchingData(t *testing.T) {
	for _, uid := range []string{"", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"} {
		t.Run("observed-"+uid, func(t *testing.T) {
			dir, original, token := restorePayloadFixture(t)
			script, err := nativeRestoreWriteScript(original, token, restorePayloadPodUID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = runRestorePayloadScriptWithUID(t, dir, script, uid); err == nil {
				t.Fatal("the native write accepted a missing or replaced consumer UID")
			}
			if _, err = os.Lstat(filepath.Join(dir, "reattach-token")); !os.IsNotExist(err) {
				t.Fatal("a different consumer was allowed to create the write token", err)
			}
		})
	}
}

func TestNativeRestoreWriteRejectsUnsafeOrUnboundUIDInput(t *testing.T) {
	for _, uid := range []string{"", "pod-name", restorePayloadPodUID + "\n", "'$(touch /data/other)'", "../other", "11111111-2222-3333-4444-55555555555F"} {
		if script, err := nativeRestoreWriteScript(strings.Repeat("a", 64), strings.Repeat("b", 64), uid); err == nil || script != "" {
			t.Fatal("an unbound or shell-active UID produced a native write script")
		}
	}
}

func restorePodUIDEnvironmentFixture() []any {
	return []any{object{"name": "LONGHORN_NATIVE_RESTORE_POD_UID", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.uid"}}}}
}

func TestNativeRestoreConsumerRequiresOnlyItsActualDownwardAPIIdentity(t *testing.T) {
	for _, reattached := range []bool{false, true} {
		for _, fault := range []string{"", "missing", "literal UID", "other field", "secret", "extra variable", "extra source", "ambiguous version"} {
			t.Run(map[bool]string{false: "restored/", true: "reattached/"}[reattached]+fault, func(t *testing.T) {
				r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, reattached)
				container := value(pod, "spec", "containers").([]any)[0].(object)
				env := restorePodUIDEnvironmentFixture()
				variable := env[0].(object)
				switch fault {
				case "missing":
					env = nil
				case "literal UID":
					delete(variable, "valueFrom")
					variable["value"] = text(pod, "metadata", "uid")
				case "other field":
					valueObject(variable, "valueFrom", "fieldRef")["fieldPath"] = "metadata.name"
				case "secret":
					variable["valueFrom"] = object{"secretKeyRef": object{"name": "application", "key": "token"}}
				case "extra variable":
					env = append(env, object{"name": "OTHER", "value": "extra"})
				case "extra source":
					variable["value"] = text(pod, "metadata", "uid")
				case "ambiguous version":
					valueObject(variable, "valueFrom", "fieldRef")["apiVersion"] = "other"
				}
				container["env"] = env
				before, err := json.Marshal(pod)
				if err != nil {
					t.Fatal(err)
				}
				err = verifyNativeRestoreConsumer(r, seed, state, pod, digest)
				if (err != nil) != (fault != "") {
					t.Fatalf("restore identity environment %q has the wrong verdict: %v", fault, err)
				}
				after, marshalErr := json.Marshal(pod)
				if marshalErr != nil || string(after) != string(before) {
					t.Fatal("consumer qualification changed the observed Pod")
				}
			})
		}
	}
}

func TestNativeRestoreBuildersGiveBothConsumersTheirRuntimeUID(t *testing.T) {
	r, seed := seedStateFixture(t, "source-cleaned")
	assets, err := nativeRestoreAssets(r, seed, "5678", nativeReattachFixture().Targets)
	if err != nil {
		t.Fatal(err)
	}
	var environment any
	for _, asset := range assets {
		if text(asset, "kind") != "ConfigMap" {
			continue
		}
		var modifiers, patch object
		if err = json.Unmarshal([]byte(text(asset, "data", "modifiers.json")), &modifiers); err != nil {
			t.Fatal(err)
		}
		rule := value(modifiers, "resourceModifierRules").([]any)[0].(object)
		merge := value(rule, "mergePatches").([]any)[0].(object)
		if err = json.Unmarshal([]byte(text(merge, "patchData")), &patch); err != nil {
			t.Fatal(err)
		}
		environment = value(value(patch, "spec", "containers").([]any)[0].(object), "env")
	}
	if !reflect.DeepEqual(environment, restorePodUIDEnvironmentFixture()) {
		t.Fatal("the restored consumer cannot identify its runtime UID")
	}
	pod, err := nativeRestoreReattachPod(r, seed, nativeReattachFixture())
	if err != nil || !reflect.DeepEqual(value(value(pod, "spec", "containers").([]any)[0].(object), "env"), restorePodUIDEnvironmentFixture()) {
		t.Fatal("the reattachment consumer cannot identify its runtime UID", err)
	}
}
