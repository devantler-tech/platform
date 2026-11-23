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

type benchmarkFenceInstallFake struct {
	t                                  *testing.T
	source, name, fault, foreignIntent string
	stored                             benchmarkRecord
	version                            int
	policy, binding                    object
	events                             []string
}

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
			fake := &benchmarkFenceInstallFake{
				t: t, source: record.Source, name: name, fault: fault,
				foreignIntent: text(policies[1], "metadata", "name") + "/policy",
				stored:        record, version: 1, events: []string{},
			}
			if fault == "concurrent journal" {
				fake.stored.FencePending = fake.foreignIntent
			}
			c := client{context: "admin@prod", run: fake.run}
			err = installBenchmarkFence(t.Context(), c, &record, desired)
			fake.assertResult(c, record, desired, err)
		})
	}
}

func (f *benchmarkFenceInstallFake) run(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.t.Helper()
	if program == "git" {
		if !slices.Equal(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"}) {
			f.t.Fatal("unexpected source read")
		}
		return []byte(f.source + "\trefs/heads/main\n"), nil
	}
	if program != "kubectl" || len(args) < 4 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=20s" {
		f.t.Fatal("unexpected command boundary")
	}
	switch args[2] {
	case "get":
		return f.get(args[3])
	case "patch":
		if args[3] != "configmap" {
			f.t.Fatal("journal patch targeted another resource")
		}
		return f.patchJournal(input)
	case "create":
		return f.create(input)
	}
	f.t.Fatalf("unexpected command %s %v", program, args)
	return nil, errors.New("unexpected command")
}

func (f *benchmarkFenceInstallFake) get(resourceKind string) ([]byte, error) {
	f.t.Helper()
	switch resourceKind {
	case "configmap":
		cm := resource("longhorn-v2-benchmark-v1")
		cm["metadata"].(object)["resourceVersion"] = strconv.Itoa(f.version)
		raw, err := json.Marshal(f.stored)
		if err != nil {
			f.t.Fatal(err)
		}
		cm["data"] = object{"record": string(raw)}
		return json.Marshal(cm)
	case "validatingadmissionpolicies.admissionregistration.k8s.io":
		return json.Marshal(f.policy)
	case "validatingadmissionpolicybindings.admissionregistration.k8s.io":
		return json.Marshal(f.binding)
	}
	f.t.Fatalf("unexpected resource %s", resourceKind)
	return nil, errors.New("unexpected resource")
}

func (f *benchmarkFenceInstallFake) patchJournal(input []byte) ([]byte, error) {
	f.t.Helper()
	var patch []object
	if json.Unmarshal(input, &patch) != nil || len(patch) != 3 ||
		text(patch[0], "op") != "test" || text(patch[0], "path") != "/metadata/uid" || value(patch[0], "value") != "uid-longhorn-v2-benchmark-v1" ||
		text(patch[1], "op") != "test" || text(patch[1], "path") != "/metadata/resourceVersion" || value(patch[1], "value") != strconv.Itoa(f.version) ||
		text(patch[2], "op") != "replace" || text(patch[2], "path") != "/data/record" {
		f.t.Fatal("journal write lost its atomic UID/resourceVersion preconditions")
	}
	var next benchmarkRecord
	if json.Unmarshal([]byte(text(patch[2], "value")), &next) != nil {
		f.t.Fatal("invalid journal write")
	}
	stage := "policy-receipt"
	if next.FencePending == f.name+"/policy" {
		stage = "policy-intent"
	} else if next.FencePending == f.name+"/binding" {
		stage = "binding-intent"
	} else if next.Fences[f.name].BindingUID != "" {
		stage = "binding-receipt"
	}
	if f.fault == "policy receipt write" && stage == "policy-receipt" || f.fault == "binding receipt write" && stage == "binding-receipt" {
		return nil, errors.New("journal write refused")
	}
	f.stored, f.version = next, f.version+1
	f.events = append(f.events, "journal:"+stage)
	return nil, nil
}

func (f *benchmarkFenceInstallFake) create(input []byte) ([]byte, error) {
	f.t.Helper()
	var created object
	if json.Unmarshal(input, &created) != nil {
		f.t.Fatal("invalid create")
	}
	kind := "policy"
	uid := "policy-uid"
	if text(created, "kind") == "ValidatingAdmissionPolicyBinding" {
		kind, uid = "binding", "binding-uid"
		if f.stored.Fences[f.name].PolicyUID != "policy-uid" {
			f.t.Fatal("binding created before the original policy UID was persisted")
		}
	}
	if f.stored.FencePending != f.name+"/"+kind {
		f.t.Fatal("create preceded its persisted ownership intent")
	}
	created["metadata"].(object)["uid"] = uid
	created["metadata"].(object)["resourceVersion"] = "17"
	if kind == "policy" {
		f.policy = created
	} else {
		f.binding = created
	}
	f.events = append(f.events, "create:"+kind)
	if f.fault == "ambiguous "+kind+" create" {
		return nil, errors.New("create response lost after server accepted it")
	}
	return json.Marshal(created)
}

func (f *benchmarkFenceInstallFake) assertResult(c client, record benchmarkRecord, desired object, err error) {
	f.t.Helper()
	want := []string{"journal:policy-intent", "create:policy", "journal:policy-receipt", "journal:binding-intent", "create:binding", "journal:binding-receipt"}
	switch f.fault {
	case "ambiguous policy create", "policy receipt write":
		want = want[:2]
	case "ambiguous binding create", "binding receipt write":
		want = want[:5]
	case "concurrent journal":
		want = want[:0]
	}
	if !slices.Equal(f.events, want) || (err == nil) != (f.fault == "") {
		f.t.Fatalf("installation order or outcome changed: events=%v err=%v", f.events, err)
	}
	if f.fault == "concurrent journal" {
		if record.FencePending != "" || f.stored.FencePending != f.foreignIntent {
			f.t.Fatal("concurrent owner journal was overwritten")
		}
		return
	}
	if !reflect.DeepEqual(record, f.stored) {
		f.t.Fatal("in-memory ownership advanced beyond the persisted journal")
	}
	if f.fault == "" {
		if record.FencePending != "" || record.Fences[f.name] != (benchmarkFenceReceipt{PolicyUID: "policy-uid", BindingUID: "binding-uid"}) {
			f.t.Fatal("complete original UID receipts were not retained")
		}
		return
	}
	before := slices.Clone(f.events)
	if installBenchmarkFence(f.t.Context(), c, &record, desired) == nil || !slices.Equal(f.events, before) {
		f.t.Fatal("unreceipted create was retried or adopted")
	}
}
