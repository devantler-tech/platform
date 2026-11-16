package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

func TestBenchmarkFenceInstallationPersistsOwnershipBeforeDependentWrites(t *testing.T) {
	for _, fault := range []string{"", "ambiguous policy create", "policy receipt write", "ambiguous binding create", "binding receipt write", "concurrent journal"} {
		t.Run(fault, func(t *testing.T) {
			record := retirementFenceFixture(t)
			for key, val := range dispatchFixture() {
				t.Setenv(key, val)
			}
			t.Setenv("GITHUB_SHA", record.Source)
			t.Setenv("LONGHORN_V2_OPERATION", "benchmark-v1")
			t.Setenv("LONGHORN_V2_CONFIRM", "benchmark-longhorn-storage")
			policies, err := benchmarkRetirementFences(record)
			if err != nil {
				t.Fatal(err)
			}
			desired := policies[0]
			name := text(desired, "metadata", "name")
			stored := record
			if fault == "concurrent journal" {
				stored.FencePending = text(policies[1], "metadata", "name") + "/policy"
			}
			version := 1
			var policy, binding object
			events := []string{}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					if !slices.Equal(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"}) {
						t.Fatal("unexpected source read")
					}
					return []byte(record.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 4 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=20s" {
					t.Fatal("unexpected command boundary")
				}
				switch args[2] {
				case "get":
					switch args[3] {
					case "configmap":
						cm := resource("longhorn-v2-benchmark-v1")
						cm["metadata"].(object)["resourceVersion"] = strconv.Itoa(version)
						raw, err := json.Marshal(stored)
						if err != nil {
							t.Fatal(err)
						}
						cm["data"] = object{"record": string(raw)}
						return json.Marshal(cm)
					case "validatingadmissionpolicies.admissionregistration.k8s.io":
						return json.Marshal(policy)
					case "validatingadmissionpolicybindings.admissionregistration.k8s.io":
						return json.Marshal(binding)
					}
				case "patch":
					var patch []object
					if args[3] != "configmap" || json.Unmarshal(input, &patch) != nil || len(patch) != 3 ||
						text(patch[0], "op") != "test" || text(patch[0], "path") != "/metadata/uid" || value(patch[0], "value") != "uid-longhorn-v2-benchmark-v1" ||
						text(patch[1], "op") != "test" || text(patch[1], "path") != "/metadata/resourceVersion" || value(patch[1], "value") != strconv.Itoa(version) ||
						text(patch[2], "op") != "replace" || text(patch[2], "path") != "/data/record" {
						t.Fatal("journal write lost its atomic UID/resourceVersion preconditions")
					}
					var next benchmarkRecord
					if json.Unmarshal([]byte(text(patch[2], "value")), &next) != nil {
						t.Fatal("invalid journal write")
					}
					stage := "policy-receipt"
					if next.FencePending == name+"/policy" {
						stage = "policy-intent"
					} else if next.FencePending == name+"/binding" {
						stage = "binding-intent"
					} else if next.Fences[name].BindingUID != "" {
						stage = "binding-receipt"
					}
					if fault == "policy receipt write" && stage == "policy-receipt" || fault == "binding receipt write" && stage == "binding-receipt" {
						return nil, errors.New("journal write refused")
					}
					stored, version = next, version+1
					events = append(events, "journal:"+stage)
					return nil, nil
				case "create":
					var created object
					if json.Unmarshal(input, &created) != nil {
						t.Fatal("invalid create")
					}
					kind := "policy"
					uid := "policy-uid"
					if text(created, "kind") == "ValidatingAdmissionPolicyBinding" {
						kind, uid = "binding", "binding-uid"
						if stored.Fences[name].PolicyUID != "policy-uid" {
							t.Fatal("binding created before the original policy UID was persisted")
						}
					}
					if stored.FencePending != name+"/"+kind {
						t.Fatal("create preceded its persisted ownership intent")
					}
					created["metadata"].(object)["uid"] = uid
					created["metadata"].(object)["resourceVersion"] = "17"
					if kind == "policy" {
						policy = created
					} else {
						binding = created
					}
					events = append(events, "create:"+kind)
					if fault == "ambiguous "+kind+" create" {
						return nil, errors.New("create response lost after server accepted it")
					}
					return json.Marshal(created)
				}
				t.Fatalf("unexpected command %s %v", program, args)
				return nil, errors.New("unexpected command")
			}}
			err = installBenchmarkFence(t.Context(), c, &record, desired)
			want := []string{"journal:policy-intent", "create:policy", "journal:policy-receipt", "journal:binding-intent", "create:binding", "journal:binding-receipt"}
			switch fault {
			case "ambiguous policy create", "policy receipt write":
				want = want[:2]
			case "ambiguous binding create", "binding receipt write":
				want = want[:5]
			case "concurrent journal":
				want = want[:0]
			}
			if !slices.Equal(events, want) || (err == nil) != (fault == "") {
				t.Fatalf("installation order or outcome changed: events=%v err=%v", events, err)
			}
			if fault == "concurrent journal" {
				if record.FencePending != "" || stored.FencePending != text(policies[1], "metadata", "name")+"/policy" {
					t.Fatal("concurrent owner journal was overwritten")
				}
				return
			}
			if !reflect.DeepEqual(record, stored) {
				t.Fatal("in-memory ownership advanced beyond the persisted journal")
			}
			if fault == "" {
				if record.FencePending != "" || record.Fences[name] != (benchmarkFenceReceipt{PolicyUID: "policy-uid", BindingUID: "binding-uid"}) {
					t.Fatal("complete original UID receipts were not retained")
				}
				return
			}
			before := slices.Clone(events)
			if installBenchmarkFence(t.Context(), c, &record, desired) == nil || !slices.Equal(events, before) {
				t.Fatal("unreceipted create was retried or adopted")
			}
		})
	}
}
