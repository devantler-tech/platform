package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
	"github.com/devantler-tech/platform/internal/strictjson"
)

const benchmarkPVFenceMessage = "Benchmark backend references are fenced for retirement"

type benchmarkAdmissionEpoch struct {
	PolicyUID, PolicyVersion, BindingUID, BindingVersion string
}

// This proves the Retain PV policy only. Other fences and original journal
// ownership must still be re-established before a storage delete.
func proveBenchmarkPVFenceOnServingAPIs(ctx context.Context, c client, record benchmarkRecord, pv object) error {
	if err := authorizeBenchmark(os.Getenv, "v1"); err != nil {
		return err
	}
	source := os.Getenv("GITHUB_SHA")
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	policies, err := benchmarkRetirementFences(record)
	if err != nil || record.Phase != "v1" {
		return errors.New("original V1 benchmark PV fence is unverified")
	}
	policy := policies[0]
	before, err := observeBenchmarkPVFence(ctx, c, policy)
	if err != nil {
		return err
	}
	roster, err := observeBenchmarkAPIServers(ctx, c)
	if err != nil {
		return err
	}
	if err := verifyBenchmarkProviderEndpoints(ctx, os.Getenv("HCLOUD_TOKEN"), roster); err != nil {
		return err
	}
	for _, endpoint := range roster {
		direct, address, err := namespaceinventory.LoadBenchmarkAdmissionClient(os.Getenv, endpoint.ExternalIP)
		if err != nil {
			return err
		}
		proof := probeBenchmarkRetainedPVAdmission(ctx, direct, address, record, pv)
		direct.CloseIdleConnections()
		if proof != nil {
			return proof
		}
	}
	after, err := observeBenchmarkAPIServers(ctx, c)
	if err != nil || !slices.Equal(roster, after) {
		return errors.New("serving API processes changed during benchmark admission proof")
	}
	bound, err := observeBenchmarkPVFence(ctx, c, policy)
	if err != nil || before != bound {
		return errors.New("the benchmark PV fence changed during native admission proof")
	}
	return sourceCurrent(ctx, c.run, source)
}

func observeBenchmarkPVFence(ctx context.Context, c client, desired object) (benchmarkAdmissionEpoch, error) {
	unverified := errors.New("the exact Deny policy and binding for the original PV are unverified")
	name := text(desired, "metadata", "name")
	policy, err := c.get(ctx, "validatingadmissionpolicies.admissionregistration.k8s.io", "", name, false)
	if err != nil || text(policy, "metadata", "name") != name || value(policy, "metadata", "deletionTimestamp") != nil ||
		!reflect.DeepEqual(value(policy, "spec"), value(desired, "spec")) {
		return benchmarkAdmissionEpoch{}, unverified
	}
	binding, err := c.get(ctx, "validatingadmissionpolicybindings.admissionregistration.k8s.io", "", name, false)
	if err != nil || text(binding, "metadata", "name") != name || value(binding, "metadata", "deletionTimestamp") != nil ||
		text(binding, "spec", "policyName") != name || !reflect.DeepEqual(value(binding, "spec", "validationActions"), []any{"Deny"}) ||
		value(binding, "spec", "paramRef") != nil || value(binding, "spec", "matchResources") != nil {
		return benchmarkAdmissionEpoch{}, unverified
	}
	return benchmarkAdmissionEpoch{
		PolicyUID: text(policy, "metadata", "uid"), PolicyVersion: text(policy, "metadata", "resourceVersion"),
		BindingUID: text(binding, "metadata", "uid"), BindingVersion: text(binding, "metadata", "resourceVersion"),
	}, nil
}

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
