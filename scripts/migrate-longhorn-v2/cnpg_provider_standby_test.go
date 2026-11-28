package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestProviderCNPGStandbyConfirmationBindsDatabaseAndAttempt(t *testing.T) {
	const operation = "observe-provider-cnpg-v2-standby"
	for _, test := range []struct {
		name, database, ordinal, override, replacement string
		allowed                                        bool
	}{
		{name: "customer identity", database: "customer-identity-db", ordinal: "3", allowed: true},
		{name: "portal", database: "business-site-portal-db", ordinal: "7", allowed: true},
		{name: "foreign database", database: "wedding-db", ordinal: "3"},
		{name: "noncanonical ordinal", database: "customer-identity-db", ordinal: "03"},
		{name: "wrong confirmation", database: "customer-identity-db", ordinal: "3", override: "LONGHORN_V2_CONFIRM", replacement: operation + ":business-site-portal-db-3"},
		{name: "replayed run", database: "customer-identity-db", ordinal: "3", override: "GITHUB_RUN_ATTEMPT", replacement: "2"},
		{name: "branch run", database: "customer-identity-db", ordinal: "3", override: "GITHUB_REF", replacement: "refs/heads/unreviewed"},
		{name: "node selected", database: "customer-identity-db", ordinal: "3", override: "LONGHORN_V2_NODE", replacement: "worker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := dispatchFixture()
			env["LONGHORN_V2_OPERATION"] = operation
			env["LONGHORN_V2_NODE"] = ""
			env["LONGHORN_V2_DATABASE"] = test.database
			member := test.database + "-" + test.ordinal
			env["LONGHORN_V2_CONFIRM"] = operation + ":" + member
			if test.override != "" {
				env[test.override] = test.replacement
			}
			grant, err := authorizeProviderCNPGStandby(func(key string) string { return env[key] })
			if (err == nil) != test.allowed {
				t.Fatalf("authorization: %v; wanted allowed=%t", err, test.allowed)
			}
			if test.allowed && (grant.Database.Name != test.database || grant.Member != member) {
				t.Fatalf("confirmed member does not match the grant: %+v", grant)
			}
		})
	}
}

func TestProviderCNPGStandbyProfileRequiresTheExactTransition(t *testing.T) {
	source := cnpgSource{Namespace: "customer-identity", Name: "customer-identity-db"}
	profile, ok := cnpgStandbyProfileForSource(source)
	if !ok || profile.Instances != 3 || profile.Size != "10Gi" || profile.SizeBytes != "10737418240" {
		t.Fatalf("wrong recovery profile: %+v", profile)
	}
	cluster := object{
		"metadata": object{"namespace": source.Namespace, "name": source.Name},
		"spec": object{
			"instances": float64(3),
			"storage":   object{"storageClass": "longhorn-v2", "size": "10Gi", "resizeInUseVolumes": false},
		},
	}
	if _, ok := cnpgStandbyTransition(cluster); !ok {
		t.Fatal("the serving standby transition was rejected")
	}
	valueObject(cluster, "spec", "storage")["resizeInUseVolumes"] = true
	if _, ok := cnpgStandbyTransition(cluster); ok {
		t.Fatal("an unqualified storage template passed")
	}
	valueObject(cluster, "spec", "storage")["resizeInUseVolumes"] = false
	valueObject(cluster, "metadata")["namespace"] = "business-site"
	if _, ok := cnpgStandbyTransition(cluster); ok {
		t.Fatal("a different namespace passed")
	}
}

func TestProviderCNPGStandbyWorkflowEntersReviewedConfigLoader(t *testing.T) {
	const child = "PLATFORM_TEST_PROVIDER_STANDBY_MAIN"
	if os.Getenv(child) == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	if !slices.Contains(choices, providerCNPGStandbyObservation) {
		t.Fatal("the protected workflow cannot invoke the provider standby observation")
	}
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = providerCNPGStandbyObservation
	env["LONGHORN_V2_NODE"] = ""
	env["LONGHORN_V2_DATABASE"] = "business-site-portal-db"
	env["LONGHORN_V2_CONFIRM"] = providerCNPGStandbyObservation + ":business-site-portal-db-3"
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProviderCNPGStandbyWorkflowEntersReviewedConfigLoader$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), child+"=1")
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "Reviewed capacity configuration is unreadable." {
		t.Fatalf("the confirmed request did not reach the configuration loader: %v: %s", err, output)
	}
}

func TestProviderCNPGOriginalRequiresExactRetainedAttachment(t *testing.T) {
	const claimUID = "31915a8b-321e-4c0c-a6bc-cc91baf16b4b"
	const volumeUID = "456a6700-9888-44f4-8e30-aeab136f0352"
	const attachmentUID = "cbefa334-ec63-4eb7-abcd-f4ddbb265325"
	const pvName, claimName, ns = "pvc-" + claimUID, "customer-identity-db-2", "customer-identity"
	claim := object{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{"name": claimName, "namespace": ns, "uid": claimUID, "resourceVersion": "10"},
		"spec": object{"volumeName": pvName, "storageClassName": "hcloud", "accessModes": []any{"ReadWriteOnce"},
			"volumeMode": "Filesystem", "resources": object{"requests": object{"storage": "10Gi"}}},
		"status": object{"phase": "Bound", "capacity": object{"storage": "10Gi"}},
	}
	pv := object{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": object{"name": pvName, "uid": volumeUID, "resourceVersion": "20"},
		"spec": object{"storageClassName": "hcloud", "persistentVolumeReclaimPolicy": "Retain", "volumeMode": "Filesystem",
			"accessModes": []any{"ReadWriteOnce"}, "capacity": object{"storage": "10Gi"},
			"csi":      object{"driver": hetznerCSIDriver, "volumeHandle": "916230"},
			"claimRef": object{"name": claimName, "namespace": ns, "uid": claimUID}},
		"status": object{"phase": "Bound"},
	}
	ticket := object{
		"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment",
		"metadata": object{"name": "csi-valid", "uid": attachmentUID, "resourceVersion": "30"},
		"spec":     object{"source": object{"persistentVolumeName": pvName}, "nodeName": "worker", "attacher": hetznerCSIDriver},
		"status":   object{"attached": true},
	}
	got, err := bindProviderCNPGOriginal(claim, pv, []object{ticket}, "worker")
	if err != nil || got.Claim.UID != claimUID || got.PV.UID != volumeUID || got.Attachment.UID != attachmentUID || got.Handle != "916230" {
		t.Fatalf("verified provider disk was refused: %v", err)
	}
	if _, err = bindProviderCNPGOriginal(claim, pv, []object{ticket, ticket}, "worker"); err == nil {
		t.Fatal("duplicate attachment was accepted")
	}
	if _, err = bindProviderCNPGOriginal(claim, pv, []object{ticket}, "other-worker"); err == nil {
		t.Fatal("attachment on the wrong host was accepted")
	}
	valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
	if _, err = bindProviderCNPGOriginal(claim, pv, []object{ticket}, "worker"); err == nil {
		t.Fatal("a reclaimable provider volume was accepted")
	}
}
