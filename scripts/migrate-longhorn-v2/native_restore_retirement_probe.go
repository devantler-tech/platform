package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"

	"github.com/devantler-tech/platform/internal/strictjson"
)

func probeNativeRestoreReclaimPV(ctx context.Context, direct *http.Client, endpoint *url.URL, state nativeRestoreState, pv object, policy object) error {
	id := state.Resources["PersistentVolume"]
	version := text(pv, "metadata", "resourceVersion")
	if benchmarkIdentity(pv) != id || version == "" {
		return errNativeRestoreEvidence
	}
	const collection = "/api/v1/persistentvolumes"
	operations := []object{
		{"op": "test", "path": "/metadata/uid", "value": id.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": version},
		{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"},
	}
	status, response, err := benchmarkAdmissionDryRun(ctx, direct, endpoint, collection+"/"+id.Name, operations)
	spec := maps.Clone(valueObject(pv, "spec"))
	spec["persistentVolumeReclaimPolicy"] = "Delete"
	if err != nil || !nativeReclaimAdmitted(status, response, id, spec) {
		return nativeReclaimProbeFailure("pv-original-update", status)
	}
	name := "lh-reclaim-probe-" + state.Run
	alias := object{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": object{"name": name}, "spec": spec}
	status, response, err = benchmarkAdmissionRequest(ctx, direct, endpoint, collection, http.MethodPost, "application/json", alias)
	policyName := text(policy, "metadata", "name")
	if err != nil || !benchmarkDenialMatches(status, response, "", "persistentvolumes", name, policyName, policyName, benchmarkReferenceFenceMessage) {
		return nativeReclaimProbeFailure("pv-alias-create", status)
	}
	return nil
}

func probeNativeRestoreReclaimClaim(ctx context.Context, direct *http.Client, endpoint *url.URL, state nativeRestoreState, claim object, policy object) error {
	id := state.Resources["PersistentVolumeClaim"]
	version := text(claim, "metadata", "resourceVersion")
	if benchmarkIdentity(claim) != id || version == "" {
		return errNativeRestoreEvidence
	}
	collection := "/api/v1/namespaces/" + benchmarkNamespace + "/persistentvolumeclaims"
	operations := []object{
		{"op": "test", "path": "/metadata/uid", "value": id.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": version},
		{"op": "replace", "path": "/spec/volumeName", "value": state.Resources["PersistentVolume"].Name},
	}
	status, response, err := benchmarkAdmissionDryRun(ctx, direct, endpoint, collection+"/"+id.Name, operations)
	spec := valueObject(claim, "spec")
	if err != nil || !nativeReclaimAdmitted(status, response, id, spec) {
		return nativeReclaimProbeFailure("pvc-original-update", status)
	}
	name := "lh-reclaim-probe-" + state.Run
	alias := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{"name": name, "namespace": benchmarkNamespace}, "spec": spec}
	status, response, err = benchmarkAdmissionRequest(ctx, direct, endpoint, collection, http.MethodPost, "application/json", alias)
	policyName := text(policy, "metadata", "name")
	if err != nil || !benchmarkDenialMatches(status, response, "", "persistentvolumeclaims", name, policyName, policyName, benchmarkReferenceFenceMessage) {
		return nativeReclaimProbeFailure("pvc-alias-create", status)
	}
	return nil
}

// The production runner may report the failed operation and status code, never
// the API response, reference identity, or direct control-plane endpoint.
func nativeReclaimProbeFailure(phase string, status int) error {
	return fmt.Errorf("native reclaim admission was unverified: phase=%s http=%d", phase, status)
}

func nativeReclaimAdmitted(status int, raw []byte, id benchmarkResource, spec object) bool {
	var actual object
	return status == http.StatusOK && strictjson.Validate(raw, nil) == nil && json.Unmarshal(raw, &actual) == nil &&
		benchmarkIdentity(actual) == id && reflect.DeepEqual(value(actual, "spec"), spec)
}
