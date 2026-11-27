package main

import (
	"strings"
	"testing"
)

const (
	detachedFixtureClaimUID = "421852ca-547b-4fd1-b59f-88594313832e"
	detachedFixturePV       = "pvc-" + detachedFixtureClaimUID
)

type detachedV1Fixture struct {
	pv, claim, volume, tickets object
	pods, attachments          []object
}

func detachedFixture() detachedV1Fixture {
	f := detachedV1Fixture{
		pv: object{
			"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": object{"name": detachedFixturePV, "uid": "a3ac48df-928c-433b-ba6d-71e76406bc3a", "resourceVersion": "42"},
			"status":   object{"phase": "Bound"},
			"spec": object{
				"persistentVolumeReclaimPolicy": "Retain", "storageClassName": "longhorn",
				"claimRef": object{"namespace": "example", "name": "original", "uid": detachedFixtureClaimUID},
				"csi":      object{"driver": "driver.longhorn.io", "volumeHandle": detachedFixturePV},
				"capacity": object{"storage": "10Gi"},
			},
		},
		claim: object{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": object{"name": "original", "namespace": "example", "uid": detachedFixtureClaimUID, "resourceVersion": "42"},
			"spec":     object{"volumeName": detachedFixturePV, "storageClassName": "longhorn"}, "status": object{"phase": "Bound"},
		},
		volume: object{
			"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
			"metadata": object{"name": detachedFixturePV, "namespace": "longhorn-system", "uid": "c6347d4d-1c46-479f-acc9-004eaef89b36", "resourceVersion": "42"},
			"spec":     object{"dataEngine": "v1", "size": "10737418240"},
			"status":   object{"state": "detached", "restoreRequired": false},
		},
		tickets: object{
			"apiVersion": "longhorn.io/v1beta2", "kind": "VolumeAttachment",
			"metadata": object{"name": detachedFixturePV, "namespace": "longhorn-system", "uid": "ba364e86-c2f0-4bdf-a958-97ce6e7c3d1c", "resourceVersion": "42"},
			"spec":     object{"volume": detachedFixturePV, "attachmentTickets": object{}},
			"status":   object{"attachmentTicketStatuses": object{}},
		},
		pods: []object{}, attachments: []object{},
	}
	return f
}

func TestDetachedV1SourceRequiresTheExactIdleOriginal(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*detachedV1Fixture)
		want   bool
	}{
		{name: "bound original", want: true},
		{name: "bound original with a dedicated storage class", want: true, change: func(f *detachedV1Fixture) {
			valueObject(f.pv, "spec")["storageClassName"] = "dedicated-storage"
			valueObject(f.claim, "spec")["storageClassName"] = "dedicated-storage"
		}},
		{name: "bound claim with a different storage class", change: func(f *detachedV1Fixture) {
			valueObject(f.claim, "spec")["storageClassName"] = "dedicated-storage"
		}},
		{name: "released original with no current claim", want: true, change: func(f *detachedV1Fixture) {
			valueObject(f.pv, "status")["phase"] = "Released"
			f.claim = nil
		}},
		{name: "released original with a separately bound claim", want: true, change: func(f *detachedV1Fixture) {
			valueObject(f.pv, "status")["phase"] = "Released"
			valueObject(f.claim, "metadata")["uid"] = "377af0e4-97ba-41b9-a2a1-411288ecd671"
			valueObject(f.claim, "spec")["volumeName"] = "pvc-377af0e4-97ba-41b9-a2a1-411288ecd671"
			valueObject(f.claim, "spec")["storageClassName"] = "longhorn-v2"
		}},
		{name: "released but original claim UID still exists", change: func(f *detachedV1Fixture) {
			valueObject(f.pv, "status")["phase"] = "Released"
		}},
		{name: "different reclaim policy", change: func(f *detachedV1Fixture) {
			valueObject(f.pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
		}},
		{name: "different engine", change: func(f *detachedV1Fixture) {
			valueObject(f.volume, "spec")["dataEngine"] = "v2"
		}},
		{name: "logical size differs", change: func(f *detachedV1Fixture) {
			valueObject(f.volume, "spec")["size"] = "2147483648"
		}},
		{name: "volume is attaching", change: func(f *detachedV1Fixture) {
			valueObject(f.volume, "status")["state"] = "attaching"
		}},
		{name: "native request still exists", change: func(f *detachedV1Fixture) {
			valueObject(f.tickets, "spec", "attachmentTickets")["backup-in-progress"] = object{}
		}},
		{name: "unknown native collection", change: func(f *detachedV1Fixture) {
			valueObject(f.tickets, "status")["attachmentTicketStatuses"] = "unknown"
		}},
		{name: "CSI attachment still exists", change: func(f *detachedV1Fixture) {
			f.attachments = []object{{"spec": object{"source": object{"persistentVolumeName": detachedFixturePV}}}}
		}},
		{name: "Pod still requests the bound claim", change: func(f *detachedV1Fixture) {
			f.pods = []object{{"spec": object{"volumes": []any{object{"persistentVolumeClaim": object{"claimName": "original"}}}}}}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := detachedFixture()
			if scenario.change != nil {
				scenario.change(&f)
			}
			got, err := bindDetachedV1Source(f.pv, f.claim, f.volume, f.tickets, f.pods, f.attachments)
			if (err == nil) != scenario.want || scenario.want && !retentionDigest.MatchString(got.Digest) {
				t.Fatalf("unexpected detached source verdict: %v", err)
			}
		})
	}
}

func TestDetachedV1SourceConfirmationIsTargetOnly(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = detachedV1SourceOperation
	env["LONGHORN_V2_CONFIRM"] = detachedV1SourceOperation + ":" + detachedFixturePV
	env["LONGHORN_V2_NODE"] = ""
	read := func(key string) string { return env[key] }
	if name, err := authorizeDetachedV1Source(read); err != nil || name != detachedFixturePV {
		t.Fatalf("the exact source was rejected: %v", err)
	}
	for _, confirmation := range []string{detachedV1SourceOperation, detachedV1SourceOperation + ":pvc-" + strings.Repeat("f", 100), env["LONGHORN_V2_CONFIRM"] + ":extra"} {
		env["LONGHORN_V2_CONFIRM"] = confirmation
		if _, err := authorizeDetachedV1Source(read); err == nil {
			t.Fatal("the exact source selector accepted a malformed confirmation")
		}
	}
	env["LONGHORN_V2_CONFIRM"] = detachedV1SourceOperation + ":" + detachedFixturePV
	env["LONGHORN_V2_DATABASE"] = "unexpected-database"
	if _, err := authorizeDetachedV1Source(read); err == nil {
		t.Fatal("a database selector was accepted in the detached volume route")
	}
}

func TestDetachedV1SourceReaderRejectsUnrelatedObjectsAndWrites(t *testing.T) {
	for _, args := range [][]string{
		{"--context=admin@prod", "--request-timeout=20s", "get", "secrets", "--output=json"},
		{"--context=admin@prod", "--request-timeout=20s", "delete", "persistentvolume", detachedFixturePV},
	} {
		_, err := detachedV1SourceReadCommand(t.Context(), nil, "kubectl", args...)
		if err == nil || err.Error() != "detached V1 qualification permits only source and Kubernetes reads" {
			t.Fatalf("unexpected command verdict: %v", err)
		}
	}
}
