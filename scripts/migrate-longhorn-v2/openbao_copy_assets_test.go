package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestOpenBaoStagePreparesOnlyTheTwoDestinations(t *testing.T) {
	pair := openBaoReboundPairFixture(t)
	first := pair[0].Intent
	claims := openBaoStagingClaims(first.Run, first.SourceSHA)
	for i, claim := range claims {
		intent := pair[i].Intent
		if text(claim, "metadata", "name") != intent.StagingClaim.Name ||
			text(claim, "metadata", "namespace") != openBaoNamespace ||
			text(claim, "spec", "storageClassName") != "longhorn-v2" ||
			providerVolumeBytes(text(claim, "spec", "resources", "requests", "storage")) != intent.Source.Bytes ||
			value(claim, "spec", "volumeName") != nil || value(claim, "spec", "dataSource") != nil {
			t.Fatalf("unexpected staging claim for the selected disk %d", i)
		}
	}

	pod := (openBaoPairPlan{Run: first.Run, Source: first.SourceSHA, Original: openBaoProviderStorage{Node: "worker-1"}}).stagePod()
	spec := valueObject(pod, "spec")
	if value(spec, "securityContext", "fsGroup") != float64(1000) ||
		text(spec, "securityContext", "fsGroupChangePolicy") != "OnRootMismatch" ||
		value(spec, "automountServiceAccountToken") != false {
		t.Fatal("staging must prepare the destination group without exposing a token")
	}
	volumes := value(spec, "volumes").([]any)
	if len(volumes) != len(claims) {
		t.Fatal("staging must mount only its two new claims")
	}
	for i, volume := range volumes {
		if text(volume.(object), "persistentVolumeClaim", "claimName") != text(claims[i], "metadata", "name") {
			t.Fatalf("staging mount %d does not bind the prepared claim", i)
		}
	}
}

func TestOpenBaoCopyNeverChangesOriginalVolumeOwnership(t *testing.T) {
	pair := openBaoReboundPairFixture(t)
	first := pair[0].Intent
	pod := (openBaoPairPlan{Run: first.Run, Source: first.SourceSHA, Input: openBaoPairInput{Member: "openbao-1"}, Original: openBaoProviderStorage{Node: "worker-1"}}).copyPod()
	spec := valueObject(pod, "spec")
	security := valueObject(spec, "securityContext")
	if security["fsGroup"] != nil || security["fsGroupChangePolicy"] != nil ||
		security["runAsUser"] != float64(100) || security["runAsGroup"] != float64(1000) ||
		spec["automountServiceAccountToken"] != false {
		t.Fatal("the fenced copy must access both original filesystems without group ownership mutation")
	}
	if value(spec, "hostNetwork") == true || value(spec, "hostUsers") != false {
		t.Fatal("copy pod isolation changed")
	}
	volumes := value(spec, "volumes").([]any)
	init := value(spec, "initContainers").([]any)
	if len(init) != 1 || len(volumes) != 5 || !reflect.DeepEqual(value(init[0].(object), "volumeMounts"), []any{
		object{"name": "destination-data", "mountPath": "/destination-data"},
		object{"name": "destination-audit", "mountPath": "/destination-audit"},
		object{"name": "source-data", "mountPath": "/source-data", "readOnly": true},
		object{"name": "source-audit", "mountPath": "/source-audit", "readOnly": true},
		object{"name": "gate", "mountPath": "/gate", "readOnly": true},
	}) {
		t.Fatal("the copy must mount exactly two original, two destination and one gate path")
	}
	for i, intent := range pair {
		original := volumes[i+2].(object)
		if text(original, "persistentVolumeClaim", "claimName") != intent.Intent.Source.PVC.Name ||
			value(original, "persistentVolumeClaim", "readOnly") != true {
			t.Fatalf("copy source %d must be the read-only original", i)
		}
	}
	cmd := value(init[0].(object), "command").([]any)
	if !strings.Contains(cmd[2].(string), "OPENBAO_OFFLINE_COPY=WAITING_FOR_ISOLATION") ||
		!strings.HasSuffix(cmd[2].(string), openBaoCopyPayload) || cmd[4] != first.Run+":"+first.SourceSHA {
		t.Fatal("the reviewed payload must follow the exact run's isolation gate")
	}
}

func TestOpenBaoCopyUsesTheRunScopedDenyPolicyAndStartsHeld(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	pod := plan.copyPod()
	policy := openBaoCopyIsolation.denyPolicy(plan.Run, plan.Source, openBaoCopyFenceOwner)
	selector := value(policy, "spec", "endpointSelector", "matchLabels")
	if !reflect.DeepEqual(selector, value(pod, "metadata", "labels")) ||
		!reflect.DeepEqual(value(policy, "spec", "ingressDeny"), []any{object{"fromEntities": []any{"all"}}}) ||
		!reflect.DeepEqual(value(policy, "spec", "egressDeny"), []any{object{"toEntities": []any{"all"}}}) ||
		text(policy, "metadata", "namespace") != openBaoNamespace {
		t.Fatal("copy traffic is not denied for the exact operation")
	}
	gate := openBaoCopyGate(plan.Run, plan.Source)
	volumes := value(pod, "spec", "volumes").([]any)
	if text(gate, "metadata", "name") != text(volumes[4].(object), "configMap", "name") ||
		text(gate, "metadata", "annotations", openBaoCopyFenceOwner) != plan.Run+":"+plan.Source ||
		text(gate, "data", "state") != "hold" {
		t.Fatal("a copy may not start before its exact isolation gate is released")
	}
}
