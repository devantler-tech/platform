package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBenchmarkPVFenceRequiresExactPolicyAndDenyBinding(t *testing.T) {
	for _, fault := range []string{"", "policy changed", "policy terminating", "binding narrowed", "binding warns", "different policy"} {
		t.Run(fault, func(t *testing.T) {
			policies, err := benchmarkRetirementFences(retirementFenceFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			desired := policies[0]
			name := text(desired, "metadata", "name")
			policy := object{"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicy", "metadata": object{"name": name, "uid": "original-policy", "resourceVersion": "100"}, "spec": maps.Clone(valueObject(desired, "spec"))}
			binding := benchmarkDenyBinding(name, "original-policy")
			valueObject(binding, "metadata")["uid"] = "original-binding"
			valueObject(binding, "metadata")["resourceVersion"] = "101"
			switch fault {
			case "policy changed":
				valueObject(policy, "spec")["failurePolicy"] = "Ignore"
			case "policy terminating":
				valueObject(policy, "metadata")["deletionTimestamp"] = "2026-01-01T00:00:00Z"
			case "binding narrowed":
				valueObject(binding, "spec")["matchResources"] = object{"namespaceSelector": object{"matchLabels": object{"missing": "true"}}}
			case "binding warns":
				valueObject(binding, "spec")["validationActions"] = []any{"Warn"}
			case "different policy":
				valueObject(binding, "spec")["policyName"] = "unrelated"
			}
			c := client{context: "admin@prod", run: func(_ context.Context, data []byte, program string, args ...string) ([]byte, error) {
				if data != nil || program != "kubectl" || len(args) != 6 ||
					!slices.Equal(args[:3], []string{"--context=admin@prod", "--request-timeout=20s", "get"}) ||
					!slices.Equal(args[4:], []string{name, "--output=json"}) {
					return nil, errors.New("unexpected benchmark fence read")
				}
				switch args[3] {
				case "validatingadmissionpolicies.admissionregistration.k8s.io":
					return json.Marshal(policy)
				case "validatingadmissionpolicybindings.admissionregistration.k8s.io":
					return json.Marshal(binding)
				}
				return nil, errors.New("unexpected benchmark fence kind")
			}}
			got, err := observeBenchmarkReferenceFence(context.Background(), c, desired, benchmarkFenceReceipt{PolicyUID: "original-policy", BindingUID: "original-binding"})
			if fault != "" && err == nil || fault == "" && (err != nil || got != (benchmarkAdmissionEpoch{"original-policy", "100", "original-binding", "101"})) {
				t.Fatalf("policy and binding readback mismatch: %v", err)
			}
		})
	}
}

func TestBenchmarkRetainedPVAdmissionRequiresBothDryRuns(t *testing.T) {
	for _, test := range []struct {
		name, fault string
		proven      bool
	}{
		{name: "both exact dry-runs", proven: true},
		{name: "identity changes on allowed response", fault: "positive-uid"},
		{name: "generic rejection does not prove fence", fault: "generic-denial"},
		{name: "unfenced server accepts the negative", fault: "negative-allowed"},
		{name: "changed original fails before either request", fault: "original-uid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := retirementFenceFixture(t)
			pv := record.Resources["PersistentVolume"]
			current := object{
				"apiVersion": "v1", "kind": "PersistentVolume",
				"metadata": object{"name": pv.Name, "uid": pv.UID, "resourceVersion": "100"},
				"spec": object{
					"persistentVolumeReclaimPolicy": "Retain", "storageClassName": record.Resources["StorageClass"].Name,
					"csi":      object{"driver": "driver.longhorn.io", "volumeHandle": record.Resources["LonghornVolume"].Name},
					"claimRef": object{"uid": record.Resources["PersistentVolumeClaim"].UID, "name": "fio-data", "namespace": record.Namespace},
				},
			}
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := calls.Add(1)
				if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/persistentvolumes/"+pv.Name || r.URL.RawQuery != "dryRun=All" ||
					r.Header.Get("Content-Type") != "application/json-patch+json" {
					t.Error("probe did not use the fixed server-side dry-run endpoint")
				}
				var changes []object
				if err := json.NewDecoder(r.Body).Decode(&changes); err != nil || len(changes) != 3 ||
					!reflect.DeepEqual(changes[:2], []object{
						{"op": "test", "path": "/metadata/uid", "value": pv.UID},
						{"op": "test", "path": "/metadata/resourceVersion", "value": "100"},
					}) || text(changes[2], "op") != "replace" || text(changes[2], "path") != "/spec/persistentVolumeReclaimPolicy" {
					t.Error("probe lacks the original UID and resource-version tests")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if attempt == 1 {
					if text(changes[2], "value") != "Retain" {
						t.Error("positive request changed the Retain policy")
					}
					reply := maps.Clone(current)
					if test.fault == "positive-uid" {
						metadata := maps.Clone(valueObject(current, "metadata"))
						metadata["uid"] = "replacement"
						reply["metadata"] = metadata
					}
					_ = json.NewEncoder(w).Encode(reply)
					return
				}
				if text(changes[2], "value") != "Delete" {
					t.Error("negative request does not challenge the fence")
				}
				if test.fault == "negative-allowed" {
					_ = json.NewEncoder(w).Encode(current)
					return
				}
				policy := "lh-benchmark-retire-pv-" + record.Phase + "-" + record.Run
				cause := "ValidatingAdmissionPolicy '" + policy + "' with binding '" + policy + "' denied request: Benchmark backend references are fenced for retirement"
				status := metav1.Status{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
					Status:   metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
					Message: "persistentvolumes " + strconv.Quote(pv.Name) + " is forbidden: " + cause,
					Details: &metav1.StatusDetails{Name: pv.Name, Kind: "persistentvolumes", Causes: []metav1.StatusCause{{Message: cause}}},
				}
				if test.fault == "generic-denial" {
					status.Details.Causes = nil
				}
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(status)
			}))
			t.Cleanup(server.Close)
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if test.fault == "original-uid" {
				valueObject(current, "metadata")["uid"] = "replacement"
			}
			err = probeBenchmarkRetainedPVAdmission(context.Background(), server.Client(), endpoint, record, current)
			if (err == nil) != test.proven || test.proven && calls.Load() != 2 || test.fault == "original-uid" && calls.Load() != 0 {
				t.Fatalf("proven = %t, calls = %d, error = %v", test.proven, calls.Load(), err)
			}
		})
	}
}

func TestBenchmarkBackendAdmissionProbesOnlyNaturallyClearedBackend(t *testing.T) {
	for _, fault := range []string{"", "still references PV", "deny from other policy"} {
		t.Run(fault, func(t *testing.T) {
			record := retirementFenceFixture(t)
			identity := record.Resources["LonghornVolume"]
			backend := object{
				"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
				"metadata": object{"name": identity.Name, "namespace": identity.Namespace, "uid": identity.UID, "resourceVersion": "100"},
				"spec":     object{"dataEngine": record.Phase},
				"status":   object{"state": "detached", "kubernetesStatus": object{}},
			}
			if fault == "still references PV" {
				valueObject(backend, "status", "kubernetesStatus")["pvName"] = record.Resources["PersistentVolume"].Name
			}
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				attempt := calls.Add(1)
				var changes []object
				if req.Method != http.MethodPatch || req.URL.RawQuery != "dryRun=All" ||
					req.URL.Path != "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/"+identity.Name ||
					json.NewDecoder(req.Body).Decode(&changes) != nil || len(changes) != 3 ||
					text(changes[0], "value") != identity.UID || text(changes[1], "value") != "100" {
					t.Error("unexpected backend probe")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if attempt == 1 {
					if text(changes[2], "path") != "/spec/dataEngine" || text(changes[2], "value") != record.Phase {
						t.Error("allowed probe attempted a changed backend")
					}
					_ = json.NewEncoder(w).Encode(backend)
					return
				}
				if text(changes[2], "path") != "/spec/nodeID" || text(changes[2], "value") != "prod-worker-3" {
					t.Error("negative probe did not challenge the detached fence")
				}
				policy := "lh-benchmark-retire-backend-" + record.Phase + "-" + record.Run
				if fault == "deny from other policy" {
					policy = "different-policy"
				}
				cause := "ValidatingAdmissionPolicy '" + policy + "' with binding '" + policy + "' denied request: " + benchmarkReferenceFenceMessage
				status := metav1.Status{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
					Status:   metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
					Message: "volumes.longhorn.io " + strconv.Quote(identity.Name) + " is forbidden: " + cause,
					Details: &metav1.StatusDetails{Group: "longhorn.io", Name: identity.Name, Kind: "volumes", Causes: []metav1.StatusCause{{Message: cause}}},
				}
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(status)
			}))
			t.Cleanup(server.Close)
			address, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = probeBenchmarkDetachedBackendAdmission(context.Background(), server.Client(), address, record, backend)
			if (fault == "") != (err == nil) || fault == "" && calls.Load() != 2 || fault == "still references PV" && calls.Load() != 0 {
				t.Fatalf("unexpected backend fence verdict: requests=%d error=%v", calls.Load(), err)
			}
		})
	}
}
