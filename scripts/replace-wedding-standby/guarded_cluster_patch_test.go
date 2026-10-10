//go:build apiserverpatch

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

// TestGuardedClusterPatchAppliesRealCAS exercises the pinned apiserver patch
// library behind the actual mTLS transport. Failed test operations commit nothing.
func TestGuardedClusterPatchAppliesRealCAS(t *testing.T) {
	for _, scenario := range []string{"version churn", "diagnostic churn", "exhausted", "malformed rejection", "structured rejection", "lost success", "generation ABA", "spec", "primary history", "annotations", "deletion", "health", "metadata ownership", "status ownership", "status manager", "protected status ownership", "malformed status ownership", "rebind failure", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			state := clusterResponse()
			state["metadata"].(object)["generation"] = float64(1)
			if scenario == "status ownership" || scenario == "status manager" || scenario == "protected status ownership" || scenario == "malformed status ownership" {
				state["metadata"].(object)["managedFields"] = []any{object{"manager": "manager", "subresource": "status", "operation": "Update", "fieldsV1": object{"f:status": object{}}, "time": testNow.Format(time.RFC3339)}}
			}
			original := transportJSON(t, state)
			var initial object
			if json.Unmarshal(original, &initial) != nil {
				t.Fatal("fixture is malformed")
			}
			requests, rebinds := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, config := transportFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write(transportJSON(t, state))
					return
				}
				if r.Method != http.MethodPatch || r.URL.Path != clusterAPIPath || r.URL.Query().Get("fieldManager") != fieldManager || r.URL.Query().Get("dryRun") != "" {
					t.Error("unexpected mutation shape")
				}
				requests++
				if requests == 1 && scenario != "lost success" || scenario == "exhausted" {
					state["metadata"].(object)["resourceVersion"] = fmt.Sprint(10 + requests)
				}
				if requests == 1 {
					switch scenario {
					case "diagnostic churn":
						state["status"].(object)["phaseReason"] = "Creating replica synthetic-join"
					case "generation ABA":
						state["metadata"].(object)["generation"] = float64(3)
					case "spec":
						state["spec"].(object)["imageName"] = "changed"
					case "primary history":
						state["status"].(object)["currentPrimaryTimestamp"] = "changed"
					case "annotations":
						state["metadata"].(object)["annotations"].(object)["other"] = "changed"
					case "deletion":
						state["metadata"].(object)["deletionTimestamp"] = testNow.Format(time.RFC3339)
					case "health":
						state["status"].(object)["readyInstances"] = float64(1)
					case "metadata ownership":
						state["metadata"].(object)["managedFields"] = []any{object{"manager": "foreign", "subresource": "status", "fieldsV1": object{"f:metadata": object{}}}}
					case "status ownership":
						state["metadata"].(object)["managedFields"] = []any{object{"manager": "manager", "subresource": "status", "operation": "Update", "fieldsV1": object{"f:status": object{"f:phaseReason": object{}}}, "time": testNow.Add(time.Second).Format(time.RFC3339)}}
					case "status manager":
						state["metadata"].(object)["managedFields"] = []any{object{"manager": "changed", "subresource": "status", "operation": "Update", "fieldsV1": object{"f:status": object{}}, "time": testNow.Format(time.RFC3339)}}
					case "protected status ownership":
						state["metadata"].(object)["managedFields"] = []any{object{"manager": "manager", "subresource": "status", "operation": "Update", "fieldsV1": object{"f:status": object{"f:currentPrimary": object{}}}, "time": testNow.Add(time.Second).Format(time.RFC3339)}}
					case "malformed status ownership":
						state["metadata"].(object)["managedFields"] = []any{object{"manager": "manager", "subresource": "status", "operation": "Update", "fieldsV1": object{"f:status": "unknown"}, "time": testNow.Format(time.RFC3339)}}
					}
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error("request body is incomplete")
					return
				}
				patch, err := jsonpatch.DecodePatch(body)
				if err != nil {
					t.Error("invalid patch encoding")
					return
				}
				updated, err := patch.Apply(transportJSON(t, state))
				if err != nil {
					w.WriteHeader(http.StatusUnprocessableEntity)
					if scenario == "malformed rejection" {
						_, _ = w.Write([]byte(`{"kind":"Status"`))
						return
					}
					status := object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Invalid", "code": 422, "message": "fixture-private-rejection"}
					if scenario == "structured rejection" {
						status["details"] = object{"causes": []any{object{"reason": "FieldValueInvalid", "message": "fixture-private-rejection"}}}
					}
					_, _ = w.Write(transportJSON(t, status))
					return
				}
				if json.Unmarshal(updated, &state) != nil {
					t.Error("result is not a Cluster")
				}
				if scenario == "lost success" {
					w.Header().Set("Content-Length", "100000")
					_, _ = w.Write([]byte(`{"apiVersion":`))
					return
				}
				_, _ = w.Write(updated)
			})
			var fallback atomic.Int32
			command := configuredCommand(t, config, &fallback)
			c := client{command: command}
			err := guardedClusterPatch(ctx, c, initial, func(ctx context.Context) (object, error) {
				rebinds++
				if scenario == "rebind failure" {
					return nil, fmt.Errorf("source proof failed")
				}
				if scenario == "deadline" {
					cancel()
					return initial, nil
				}
				data, e := command(ctx, clusterGetArgs(), nil)
				var cluster object
				if e == nil {
					e = json.Unmarshal(data, &cluster)
				}
				return cluster, e
			}, func(cluster object) []object { return pausePatch(cluster, str(initial, "status", "currentPrimary")) })
			ok := scenario == "version churn" || scenario == "diagnostic churn" || scenario == "structured rejection" || scenario == "status ownership"
			if (err == nil) != ok {
				t.Fatalf("scenario=%s requests=%d rebinds=%d error=%v", scenario, requests, rebinds, err)
			}
			want := 1
			if ok {
				want = 2
			} else if scenario == "exhausted" {
				want = clusterPatchAttempts
			}
			wantRebinds := 1
			if ok {
				wantRebinds = 1
			} else if scenario == "exhausted" {
				wantRebinds = clusterPatchAttempts - 1
			} else if scenario == "malformed rejection" || scenario == "lost success" {
				wantRebinds = 0
			}
			if requests != want || rebinds != wantRebinds || fallback.Load() != 0 {
				t.Fatalf("unexpected requests=%d fallback=%d", requests, fallback.Load())
			}
			if !ok && scenario != "lost success" && str(state, "metadata", "annotations", pauseKey) != "" {
				t.Fatal("rejected patch persisted a pause")
			}
			if scenario == "lost success" && (rebinds != 0 || str(state, "metadata", "annotations", pauseKey) != "disabled") {
				t.Fatal("uncertain committed response was retried")
			}
			if err != nil && strings.Contains(err.Error(), "fixture-private") {
				t.Fatal("private response escaped")
			}
		})
	}
}

// TestGuardedClusterPatchRefusesUncertainOutcomes admits only typed, complete
// native rejections; a matching text error does not establish no persistence.
func TestGuardedClusterPatchRefusesUncertainOutcomes(t *testing.T) {
	for _, reason := range []string{"SERVER_INVALID", "SERVER_CONFLICT", "SERVER_THROTTLED", "SERVER_INTERNAL", "SERVER_UNAVAILABLE", "SERVER_TIMEOUT", "UNKNOWN"} {
		t.Run(reason, func(t *testing.T) {
			calls := 0
			c := client{command: func(context.Context, []string, []byte) ([]byte, error) {
				calls++
				return nil, clusterRequestFailure{reason}
			}}
			state := clusterResponse()
			err := guardedClusterPatch(context.Background(), c, state, func(context.Context) (object, error) {
				t.Fatal("uncertain outcome re-entered guard proof")
				return nil, nil
			}, func(o object) []object { return pausePatch(o, str(o, "status", "currentPrimary")) })
			if err == nil || calls != 1 {
				t.Fatal("uncertain outcome was retried")
			}
		})
	}
}

func TestGuardedClusterPatchRefusesLookalikeText(t *testing.T) {
	calls := 0
	c := client{command: func(context.Context, []string, []byte) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("SERVER_INVALID_NO_CAUSES: synthetic text is not a verified envelope")
	}}
	state := clusterResponse()
	err := guardedClusterPatch(context.Background(), c, state, func(context.Context) (object, error) {
		t.Fatal("text-only failure re-entered guard proof")
		return nil, nil
	}, func(o object) []object { return pausePatch(o, str(o, "status", "currentPrimary")) })
	if err == nil || calls != 1 {
		t.Fatal("text-only failure was retried")
	}
}

// TestClusterStateProjectionDoesNotModifyObservation preserves the actual RV,
// complete patch body, and caller-owned metadata while comparing frozen state.
func TestClusterStateProjectionDoesNotModifyObservation(t *testing.T) {
	state := clusterResponse()
	before := transportJSON(t, state)
	if _, err := protectedClusterState(state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, transportJSON(t, state)) {
		t.Fatal("projection altered the observed version or patch predicates")
	}
}
