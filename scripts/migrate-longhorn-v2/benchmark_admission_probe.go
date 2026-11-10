package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const benchmarkPVFenceMessage = "Benchmark backend references are fenced for retirement"

func probeBenchmarkRetainedPVAdmission(ctx context.Context, client *http.Client, endpoint *url.URL, record benchmarkRecord, pv object) error {
	unverified := errors.New("the original Retain PV fence is unverified on this API server")
	identity := record.Resources["PersistentVolume"]
	version := text(pv, "metadata", "resourceVersion")
	if verifyRetirementReceipts(record) != nil || benchmarkIdentity(pv) != identity || version == "" ||
		text(pv, "apiVersion") != "v1" || text(pv, "kind") != "PersistentVolume" ||
		text(pv, "spec", "csi", "driver") != "driver.longhorn.io" ||
		text(pv, "spec", "csi", "volumeHandle") != record.Resources["LonghornVolume"].Name ||
		text(pv, "spec", "claimRef", "uid") != record.Resources["PersistentVolumeClaim"].UID ||
		text(pv, "spec", "claimRef", "name") != "fio-data" ||
		text(pv, "spec", "claimRef", "namespace") != record.Namespace ||
		text(pv, "spec", "storageClassName") != record.Resources["StorageClass"].Name ||
		text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
		return unverified
	}

	path := "/api/v1/persistentvolumes/" + identity.Name
	tested := []object{{"op": "test", "path": "/metadata/uid", "value": identity.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": version},
		{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"}}
	code, raw, err := benchmarkAdmissionDryRun(ctx, client, endpoint, path, tested)
	var admitted object
	if err != nil || code != http.StatusOK || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &admitted) != nil ||
		benchmarkIdentity(admitted) != identity || text(admitted, "apiVersion") != "v1" ||
		text(admitted, "kind") != "PersistentVolume" || !reflect.DeepEqual(value(admitted, "spec"), value(pv, "spec")) {
		return unverified
	}
	tested[2]["value"] = "Delete"
	code, raw, err = benchmarkAdmissionDryRun(ctx, client, endpoint, path, tested)
	name := "lh-benchmark-retire-pv-" + record.Phase + "-" + record.Run
	if err != nil || !benchmarkDenialMatches(code, raw, "", "persistentvolumes", identity.Name, name, name, benchmarkPVFenceMessage) {
		return unverified
	}
	return nil
}

// Keep the native response in bounded memory. The caller accepts only its
// fixed object or admission-policy status and never emits the response.
func benchmarkAdmissionDryRun(ctx context.Context, client *http.Client, endpoint *url.URL, path string, operations []object) (int, []byte, error) {
	const limit = 2 << 20
	unverified := errors.New("native admission dry-run did not return a bounded response")
	if client == nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return 0, nil, unverified
	}
	destination := url.URL{Scheme: "https", Host: endpoint.Host, Path: path, RawQuery: "dryRun=All"}
	payload, err := json.Marshal(operations)
	if err != nil {
		return 0, nil, unverified
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, destination.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, nil, unverified
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json-patch+json")
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, unverified
	}
	raw, readErr := io.ReadAll(io.LimitReader(res.Body, limit+1))
	closeErr := res.Body.Close()
	if readErr != nil || closeErr != nil || len(raw) > limit {
		return 0, nil, unverified
	}
	return res.StatusCode, raw, nil
}
