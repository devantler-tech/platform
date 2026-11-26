package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func recoveryAdmissionFixture(t *testing.T) (object, object) {
	t.Helper()
	c, store, backup := recoverySourceFixture("umami-db")
	c["spec"].(object)["imageName"] = "ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie"
	source, err := bindCNPGRecoverySource(c, store, backup, text(backup, "metadata", "uid"), archiveBindingFixture(c, store))
	if err != nil {
		t.Fatal(err)
	}
	assets, err := cnpgRecoveryResources("1234", source, "ghcr.io/cloudnative-pg/postgresql@sha256:f7e00138159bb45d70b624ba25e446924cb3ed1bf366d13e936c3f81b914064e", postgresState{SystemID: "1234567890123456789", Timeline: "7", FlushLSN: "2/100"}, "qualified-worker")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/synthetic-admitted-recovery-cnpg-1.30.2.json")
	var admitted object
	if err != nil || decodeIsolationJSON(raw, &admitted) != nil {
		t.Fatal("synthetic source-derived admission fixture")
	}
	return assets[3], admitted
}

func TestRecoveryAdmissionMatchesOnlyThePinnedNativeDefaults(t *testing.T) {
	proposal, admitted := recoveryAdmissionFixture(t)
	before, _ := json.Marshal(proposal)
	bound, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"})
	after, _ := json.Marshal(proposal)
	if err != nil || !reflect.DeepEqual(bound, value(admitted, "spec")) || string(before) != string(after) {
		t.Fatalf("pinned native default projection did not match or mutated the request: %v", err)
	}
	bound["imageName"] = "changed-after-return"
	if text(admitted, "spec", "imageName") == "changed-after-return" {
		t.Fatal("admission binding retained a mutable reference to the response")
	}
}

func TestRecoveryAdmissionRejectsInjectedBehaviorAndChangedSources(t *testing.T) {
	for name, change := range map[string]func(object){
		"enabled plugin": func(o object) { value(o, "spec", "plugins").([]any)[0].(object)["enabled"] = true },
		"parameters on disabled plugin": func(o object) {
			value(o, "spec", "plugins").([]any)[0].(object)["parameters"] = object{"serverName": "private-value"}
		},
		"extra included plugin": func(o object) {
			o["spec"].(object)["plugins"] = append(value(o, "spec", "plugins").([]any), object{"name": "unexpected.example", "enabled": true})
		},
		"legacy archive": func(o object) {
			o["spec"].(object)["backup"] = object{"barmanObjectStore": object{"destinationPath": "private-value"}}
		},
		"archive command": func(o object) {
			value(o, "spec", "postgresql", "parameters").(object)["archive_command"] = "private-value"
		},
		"extra environment": func(o object) {
			o["spec"].(object)["env"] = []any{object{"name": "UNREQUESTED", "value": "private-value"}}
		},
		"V1 storage":       func(o object) { value(o, "spec", "storage").(object)["storageClass"] = "longhorn-wffc" },
		"WAL extra volume": func(o object) { o["spec"].(object)["walStorage"] = object{"size": "5Gi"} },
		"another recovery point": func(o object) {
			value(o, "spec", "bootstrap", "recovery", "recoveryTarget").(object)["targetLSN"] = "2/200"
		},
		"another namespace":      func(o object) { o["metadata"].(object)["namespace"] = "umami" },
		"unrequested annotation": func(o object) { o["metadata"].(object)["annotations"] = object{"cnpg.io/unrequested": "private-value"} },
		"changed native default": func(o object) { o["spec"].(object)["primaryUpdateStrategy"] = "supervised" },
	} {
		t.Run(name, func(t *testing.T) {
			proposal, admitted := recoveryAdmissionFixture(t)
			change(admitted)
			bound, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"})
			if err == nil || bound != nil || strings.Contains(err.Error(), "private-value") {
				t.Fatal("unrequested admission behavior cleared or leaked")
			}
		})
	}
}

func TestRecoveryAdmissionMonitoringDefaultsNeedAnIndependentConfigurationBinding(t *testing.T) {
	for _, cm := range []bool{false, true} {
		for _, secret := range []bool{false, true} {
			proposal, admitted := recoveryAdmissionFixture(t)
			monitoring := value(admitted, "spec", "monitoring").(object)
			if cm {
				monitoring["customQueriesConfigMap"] = []any{object{"name": "cnpg-default-monitoring", "key": "queries"}}
			}
			if secret {
				monitoring["customQueriesSecret"] = []any{object{"name": "cnpg-default-monitoring", "key": "queries"}}
			}
			if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2", ConfigMapQueries: cm, SecretQueries: secret}); err != nil {
				t.Fatal("independently bound monitoring defaults did not match")
			}
			if cm || secret {
				if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"}); err == nil {
					t.Fatal("monitoring configuration was inferred from its admitted effect")
				}
			}
		}
	}
}

func TestRecoveryAdmissionRefusesAnUnboundVersionAndArbitraryProposals(t *testing.T) {
	proposal, admitted := recoveryAdmissionFixture(t)
	for _, version := range []string{"", "1.30.1", "1.30.3"} {
		if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: version}); err == nil {
			t.Fatal("an unbound controller/schema profile cleared")
		}
	}
	proposal["spec"].(object)["postgresql"] = object{"parameters": object{"archive_command": "private-value"}}
	if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"}); err == nil {
		t.Fatal("a different proposal bypassed the renderer's allowed inputs")
	}
}

func TestRecoveryAdmissionRejectsProposalsOutsideTheOwnedRestoreShape(t *testing.T) {
	for name, change := range map[string]func(object){
		"another ObjectStore": func(o object) {
			value(o, "spec", "externalClusters").([]any)[0].(object)["plugin"].(object)["parameters"].(object)["barmanObjectName"] = "another-store"
		},
		"another bootstrap mode": func(o object) { value(o, "spec", "bootstrap", "recovery").(object)["source"] = "another-source" },
		"preselected PV": func(o object) {
			value(o, "spec", "storage").(object)["pvcTemplate"] = object{"volumeName": "existing-volume"}
		},
		"extra resources": func(o object) { value(o, "spec", "resources", "requests").(object)["ephemeral-storage"] = "100Gi" },
	} {
		t.Run(name, func(t *testing.T) {
			proposal, admitted := recoveryAdmissionFixture(t)
			change(proposal)
			change(admitted)
			if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"}); err == nil {
				t.Fatal("a matching but unowned restore shape cleared")
			}
		})
	}
}

func TestRecoveryAdmissionPreservesOnlyTheRequestedNodeSelector(t *testing.T) {
	proposal, admitted := recoveryAdmissionFixture(t)
	selector := object{"kubernetes.io/hostname": "synthetic-hostname"}
	proposal["spec"].(object)["affinity"] = object{"nodeSelector": selector}
	value(admitted, "spec", "affinity").(object)["nodeSelector"] = object{"kubernetes.io/hostname": "synthetic-hostname"}
	if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"}); err != nil {
		t.Fatal("native anti-affinity default did not preserve the requested placement")
	}
	value(admitted, "spec", "affinity", "nodeSelector").(object)["kubernetes.io/hostname"] = "another-hostname"
	if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"}); err == nil {
		t.Fatal("admission moved the restore away from the qualified Node")
	}
}

func TestRecoveryAdmissionRefusesAnUnplacedRecovery(t *testing.T) {
	proposal, admitted := recoveryAdmissionFixture(t)
	delete(proposal["spec"].(object), "affinity")
	admitted["spec"].(object)["affinity"] = object{"podAntiAffinityType": "preferred"}
	if _, err := matchRecoveryAdmission(proposal, admitted, recoveryAdmissionDefaults{Version: "1.30.2"}); err == nil {
		t.Fatal("an unplaced recovery could start before its Node's datapath qualification")
	}
}
