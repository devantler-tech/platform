package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

func TestOpenBaoFenceRejectsFSGroupInjectedByAdmission(t *testing.T) {
	const policy = "openbao-copy-fence"
	plan := openBaoPairPlan{Run: "991", Source: strings.Repeat("a", 40)}
	plan.Input.Member = "openbao-1"
	plan.Original.Node = "worker-1"
	plan.Original.Disks[0].PVC.Name = "data-openbao-1"
	plan.Original.Disks[1].PVC.Name = "audit-openbao-1"
	for _, shape := range []string{"authored", "injected fsGroup"} {
		t.Run(shape, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodPost || req.URL.Query().Get("dryRun") != "All" {
					t.Error("the admission probe must be a dry-run")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var pod object
				if err := json.NewDecoder(req.Body).Decode(&pod); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				name := text(pod, "metadata", "name")
				if name == "openbao-storage-fence-probe" {
					denial := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s",
						policy, policy, openBaoCopyFenceMessage)
					w.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(w).Encode(object{"apiVersion": "v1", "kind": "Status", "code": 403,
						"status": "Failure", "reason": "Forbidden", "message": fmt.Sprintf("pods %q is forbidden: %s", name, denial),
						"details": object{"name": name, "kind": "pods", "causes": []object{{"message": denial}}}})
					return
				}
				valueObject(pod, "metadata")["uid"] = "a4f66240-e801-4624-a98a-af06d7fbfb2f"
				if shape == "injected fsGroup" {
					security := valueObject(pod, "spec", "securityContext")
					security["fsGroup"], security["fsGroupChangePolicy"] = 1000, "OnRootMismatch"
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(pod)
			}))
			defer server.Close()
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = probeOpenBaoFence(t.Context(), server.Client(), endpoint, 0, policy, plan)
			if (err == nil) != (shape == "authored") {
				t.Fatalf("dry-run receipt for %s: %v", shape, err)
			}
		})
	}
}

func TestOpenBaoCopyFenceAdmitsOnlyItsProtectedReadOnlyPair(t *testing.T) {
	const run = "12345"
	const executor = "maintenance-executor"
	owner := run + ":" + strings.Repeat("a", 40)
	policy, err := openBaoCopyFence(run, strings.Repeat("a", 40), "openbao-1", executor)
	if err != nil {
		t.Fatal(err)
	}
	checks := value(policy, "spec", "validations").([]any)
	expression := text(checks[0].(object), "expression")
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	result, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(result)
	if err != nil {
		t.Fatal(err)
	}
	mount := func(claim string, readOnly bool) object {
		return object{"persistentVolumeClaim": object{"claimName": claim, "readOnly": readOnly}}
	}
	data := mount("data-openbao-1", true)
	audit := mount("audit-openbao-1", true)
	for _, tc := range []struct {
		name, pod, user, annotation string
		volumes                     []any
		allow                       bool
	}{
		{"ordinary peer", "openbao-2", "stateful-controller", "", []any{mount("data-openbao-2", false)}, true},
		{"selected member", "openbao-1", "stateful-controller", "", []any{data, audit}, false},
		{"selected ordinal without volumes", "openbao-1", "stateful-controller", "", nil, false},
		{"foreign consumer", "unrelated", "tenant", "", []any{data}, false},
		{"owned copy", "lh-openbao-copy-12345", executor, owner, []any{data, audit}, true},
		{"different executor", "lh-openbao-copy-12345", "other", owner, []any{data, audit}, false},
		{"different ownership", "lh-openbao-copy-12345", executor, "foreign", []any{data, audit}, false},
		{"reserved name without sources", "lh-openbao-copy-12345", executor, owner, nil, false},
		{"missing audit", "lh-openbao-copy-12345", executor, owner, []any{data}, false},
		{"duplicate source", "lh-openbao-copy-12345", executor, owner, []any{data, audit, data}, false},
		{"read-write audit", "lh-openbao-copy-12345", executor, owner, []any{data, mount("audit-openbao-1", false)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := object{}
			if tc.volumes != nil {
				spec["volumes"] = tc.volumes
			}
			meta := object{"name": tc.pod}
			if tc.annotation != "" {
				meta["annotations"] = object{openBaoCopyFenceOwner: tc.annotation}
			}
			value, _, err := program.Eval(object{
				"object":  object{"metadata": meta, "spec": spec},
				"request": object{"userInfo": object{"username": tc.user}},
			})
			if err != nil || value.Value() != tc.allow {
				t.Fatalf("copy admission = %v, want %t (%v)", value, tc.allow, err)
			}
		})
	}
}

func TestOpenBaoClaimFenceAllowsOnlyThePreparedPair(t *testing.T) {
	const executor = "maintenance-executor"
	pair := openBaoReboundPairFixture(t)
	intents := [2]openBaoStorageRebind{pair[0].Intent, pair[1].Intent}
	policy, err := openBaoClaimFence("openbao-1", executor, intents)
	if err != nil {
		t.Fatal(err)
	}
	rule := value(policy, "spec", "matchConstraints", "resourceRules").([]any)[0].(object)
	if value(rule, "resources").([]any)[0] != "persistentvolumeclaims" ||
		value(rule, "operations").([]any)[0] != "CREATE" {
		t.Fatal("the claim fence must be limited to PVC creation")
	}
	validation := value(policy, "spec", "validations").([]any)[0].(object)
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	expression, issues := env.Compile(text(validation, "expression"))
	if issues != nil && issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(expression)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user string
		disk       int
		modify     func(object)
		allow      bool
	}{
		{name: "prepared data claim", user: executor, allow: true},
		{name: "prepared audit claim", user: executor, disk: 1, allow: true},
		{name: "unrelated member", user: "stateful-controller", modify: func(claim object) {
			valueObject(claim, "metadata")["name"] = "data-openbao-2"
		}, allow: true},
		{name: "controller race", user: "stateful-controller"},
		{name: "crossed destination", user: executor, modify: func(claim object) {
			valueObject(claim, "spec")["volumeName"] = intents[1].DestinationPV.Name
		}},
		{name: "original class", user: executor, disk: 1, modify: func(claim object) {
			valueObject(claim, "spec")["storageClassName"] = "hcloud"
		}},
		{name: "wrong ownership", user: executor, modify: func(claim object) {
			valueObject(claim, "metadata", "annotations")[openBaoVolumeRebindOwner] = "unknown"
		}},
		{name: "missing ownership", user: executor, modify: func(claim object) {
			delete(valueObject(claim, "metadata"), "annotations")
		}},
		{name: "new data source", user: executor, modify: func(claim object) {
			valueObject(claim, "spec")["dataSourceRef"] = object{"kind": "PersistentVolumeClaim", "name": "other"}
		}},
		{name: "different access mode", user: executor, modify: func(claim object) {
			valueObject(claim, "spec")["accessModes"] = []any{"ReadWriteMany"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := intents[tc.disk].replacementClaim()
			if tc.modify != nil {
				tc.modify(claim)
			}
			result, _, err := program.Eval(object{
				"object":  claim,
				"request": object{"userInfo": object{"username": tc.user}},
			})
			if err != nil || result.Value() != tc.allow {
				t.Fatalf("claim admission = %v, want %t (%v)", result, tc.allow, err)
			}
		})
	}
	intents[1].DestinationPV = intents[0].DestinationPV
	if _, err := openBaoClaimFence("openbao-1", executor, intents); err == nil {
		t.Fatal("two claims sharing the same prepared volume were accepted")
	}
}
