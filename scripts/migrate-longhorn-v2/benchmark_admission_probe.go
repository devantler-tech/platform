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

const benchmarkReferenceFenceMessage = "Benchmark backend references are fenced for retirement"

type benchmarkAdmissionEpoch struct {
	PolicyUID, PolicyVersion, BindingUID, BindingVersion string
}

// Prove the fence for the supplied original reference. Instance and namespace
// fences and the journal still require separate evidence before any delete.
func proveBenchmarkReferenceFenceOnServingAPIs(ctx context.Context, c client, record benchmarkRecord, observed object) error {
	if err := authorizeBenchmark(os.Getenv, "v1"); err != nil {
		return err
	}
	source := os.Getenv("GITHUB_SHA")
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	policies, err := benchmarkRetirementFences(record)
	if err != nil || record.Phase != "v1" {
		return errors.New("original V1 benchmark reference fence is unverified")
	}
	var policy object
	var probe func(context.Context, *http.Client, *url.URL, benchmarkRecord, object) error
	switch text(observed, "kind") {
	case "PersistentVolume":
		policy, probe = policies[0], probeBenchmarkRetainedPVAdmission
	case "Volume":
		policy, probe = policies[1], probeBenchmarkDetachedBackendAdmission
	default:
		return errors.New("benchmark reference fence received an unsupported resource")
	}
	before, err := observeBenchmarkReferenceFence(ctx, c, policy)
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
		proof := probe(ctx, direct, address, record, observed)
		direct.CloseIdleConnections()
		if proof != nil {
			return proof
		}
	}
	after, err := observeBenchmarkAPIServers(ctx, c)
	if err != nil || !slices.Equal(roster, after) {
		return errors.New("serving API processes changed during benchmark admission proof")
	}
	bound, err := observeBenchmarkReferenceFence(ctx, c, policy)
	if err != nil || before != bound {
		return errors.New("the benchmark reference fence changed during native admission proof")
	}
	return sourceCurrent(ctx, c.run, source)
}

func observeBenchmarkReferenceFence(ctx context.Context, c client, desired object) (benchmarkAdmissionEpoch, error) {
	unverified := errors.New("the exact Deny policy and binding for the original reference are unverified")
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
	if err != nil || !benchmarkDenialMatches(code, raw, "", "persistentvolumes", identity.Name, name, name, benchmarkReferenceFenceMessage) {
		return unverified
	}
	return nil
}

func probeBenchmarkDetachedBackendAdmission(ctx context.Context, client *http.Client, endpoint *url.URL, record benchmarkRecord, backend object) error {
	unverified := errors.New("the detached backend fence is unverified on this API server")
	identity := record.Resources["LonghornVolume"]
	const group = "longhorn.io"
	const version = group + "/v1beta2"
	resourceVersion := text(backend, "metadata", "resourceVersion")
	if verifyRetirementReceipts(record) != nil || benchmarkIdentity(backend) != identity || resourceVersion == "" ||
		text(backend, "apiVersion") != version || text(backend, "kind") != "Volume" ||
		text(backend, "spec", "dataEngine") != record.Phase ||
		verifyBenchmarkDetachedBackendState(backend) != nil ||
		verifyRecoveredBackendCascadeCleared(backend) != nil {
		return unverified
	}
	path := "/apis/" + version + "/namespaces/longhorn-system/volumes/" + identity.Name
	tested := []object{
		{"op": "test", "path": "/metadata/uid", "value": identity.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": resourceVersion},
		{"op": "replace", "path": "/spec/dataEngine", "value": record.Phase},
	}
	code, raw, err := benchmarkAdmissionDryRun(ctx, client, endpoint, path, tested)
	var admitted object
	if err != nil || code != http.StatusOK || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &admitted) != nil ||
		benchmarkIdentity(admitted) != identity || text(admitted, "apiVersion") != version || text(admitted, "kind") != "Volume" ||
		!reflect.DeepEqual(value(admitted, "spec"), value(backend, "spec")) ||
		verifyBenchmarkDetachedBackendState(admitted) != nil || verifyRecoveredBackendCascadeCleared(admitted) != nil {
		return unverified
	}
	tested[2] = object{"op": "add", "path": "/spec/nodeID", "value": "prod-worker-3"}
	code, raw, err = benchmarkAdmissionDryRun(ctx, client, endpoint, path, tested)
	name := "lh-benchmark-retire-backend-" + record.Phase + "-" + record.Run
	if err != nil || !benchmarkDenialMatches(code, raw, group, "volumes", identity.Name, name, name, benchmarkReferenceFenceMessage) {
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
