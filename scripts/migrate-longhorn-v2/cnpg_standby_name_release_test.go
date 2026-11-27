package main

import (
	"testing"

	"cel.dev/cel-go/cel"
)

func TestUmamiReplacementCanReuseOnlyAReleasedOrdinalWithNewIdentities(t *testing.T) {
	observation, pod, claim, pv, volume, pdb := umamiStandbyCutoverFixture(t)
	witness, err := bindUmamiStandbyCutover(observation, observation.Pods, text(pod, "metadata", "name"), claim, pv, volume, pdb)
	if err != nil {
		t.Fatal(err)
	}
	fresh := cnpgRecoveredStorage{
		Cluster: witness.Database,
		Pod:     benchmarkResource{witness.Consumer.Name, "umami", "ac005841-b001-4001-8001-000000000001"},
		Claim:   benchmarkResource{witness.Claim.Name, "umami", "ac005841-b002-4002-8002-000000000002"},
		PV:      benchmarkResource{"pvc-ac005841-b002-4002-8002-000000000002", "", "ac005841-b003-4003-8003-000000000003"},
		Volume:  benchmarkResource{"pvc-ac005841-b002-4002-8002-000000000002", "longhorn-system", "ac005841-b004-4004-8004-000000000004"},
		Engine:  benchmarkResource{"fresh-engine", "longhorn-system", "ac005841-b005-4005-8005-000000000005"},
		Replicas: map[string]benchmarkResource{
			"one":   {"fresh-one", "longhorn-system", "ac005841-b006-4006-8006-000000000006"},
			"two":   {"fresh-two", "longhorn-system", "ac005841-b007-4007-8007-000000000007"},
			"three": {"fresh-three", "longhorn-system", "ac005841-b008-4008-8008-000000000008"},
		},
	}
	witness.Spare.Pod.Name, witness.Spare.Claim.Name = "umami-db-4", "umami-db-4"
	witness.Spare.Pod.UID = "ac005841-a004-4004-8004-000000000004"
	witness.Spare.Claim.UID = "ac005841-a005-4005-8005-000000000005"
	witness.Spare.PV.UID = "ac005841-a006-4006-8006-000000000006"
	witness.Spare.Volume.UID = "ac005841-a007-4007-8007-000000000007"
	witness.Spare.Engine.UID = "ac005841-a008-4008-8008-000000000008"
	if independentUmamiReplacement(witness, fresh) {
		t.Fatal("a reused name was accepted before release of the old claim")
	}
	if !independentUmamiReplacementAfterRelease(witness, fresh, true) {
		t.Fatal("a fresh V2 member on the released ordinal was rejected")
	}
	fresh.Pod.UID = witness.Consumer.UID
	if independentUmamiReplacementAfterRelease(witness, fresh, true) {
		t.Fatal("the original pod was accepted as its own replacement")
	}
}

func TestUmamiReleasedVolumeRetainsOriginalClaimIdentity(t *testing.T) {
	observation, pod, claim, pv, volume, pdb := umamiStandbyCutoverFixture(t)
	witness, err := bindUmamiStandbyCutover(observation, observation.Pods, text(pod, "metadata", "name"), claim, pv, volume, pdb)
	if err != nil {
		t.Fatal(err)
	}
	valueObject(pv, "status")["phase"] = "Released"
	status := valueObject(volume, "status")
	status["state"], status["robustness"], status["currentNodeID"] = "detached", "unknown", ""
	kubernetes := valueObject(status, "kubernetesStatus")
	kubernetes["pvcName"], kubernetes["namespace"] = "", ""
	if !umamiReleasedVolumeMatches(witness, pv, volume) {
		t.Fatal("the original released Retain backend was rejected")
	}
	for field, changed := range map[string]string{"uid": freshTestUID, "namespace": "other", "name": "foreign"} {
		ref := valueObject(pv, "spec", "claimRef")
		previous := ref[field]
		ref[field] = changed
		if umamiReleasedVolumeMatches(witness, pv, volume) {
			t.Errorf("accepted a released PV with changed claim %s", field)
		}
		ref[field] = previous
	}
	valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
	if umamiReleasedVolumeMatches(witness, pv, volume) {
		t.Fatal("accepted releasing a volume without Retain")
	}
}

const freshTestUID = "ac005841-f999-4999-8999-000000000999"

func TestUmamiNameFenceCoversOnlyTheRetiredMember(t *testing.T) {
	const target = "umami-db-1"
	record := umamiStandbyReplacementRecord{
		Original:    cnpgStandbyCutover{Claim: benchmarkResource{Name: target}},
		NameRelease: &umamiStandbyNameRelease{Owner: umamiReplacementOwner{Run: "1234567"}},
	}
	policy := umamiNameFencePolicy(record)
	conditions := value(policy, "spec", "matchConditions").([]any)
	validations := value(policy, "spec", "validations").([]any)
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	expression := "!(" + text(conditions[0].(object), "expression") + ") || (" + text(validations[0].(object), "expression") + ")"
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	mount := map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": target}}}}
	for _, check := range []struct {
		resource, namespace, name string
		spec                      map[string]any
		admit                     bool
	}{
		{"pods", "umami", target, nil, false},
		{"pods", "umami", "foreign-pod", mount, false},
		{"pods", "umami", "", mount, false},
		{"pods", "umami", "", map[string]any{"volumes": []any{map[string]any{"emptyDir": map[string]any{}}}}, true},
		{"pods", "umami", "umami-db-2", nil, true},
		{"pods", "elsewhere", target, mount, true},
		{"jobs", "umami", target + "-join", nil, false},
		{"jobs", "umami", target + "-restore", nil, false},
		{"jobs", "umami", "foreign-job", map[string]any{"template": map[string]any{"spec": mount}}, false},
		{"jobs", "umami", "umami-db-2-join", map[string]any{"template": map[string]any{"spec": map[string]any{}}}, true},
		{"persistentvolumeclaims", "umami", target, nil, false},
		{"persistentvolumeclaims", "umami", "umami-db-2", nil, true},
	} {
		metadata := map[string]any{}
		if check.name != "" {
			metadata["name"] = check.name
		}
		if check.spec == nil {
			check.spec = map[string]any{}
		}
		got, _, evalErr := program.Eval(map[string]any{
			"request": map[string]any{"namespace": check.namespace, "resource": map[string]any{"resource": check.resource}},
			"object":  map[string]any{"metadata": metadata, "spec": check.spec},
		})
		if evalErr != nil || got.Value() != check.admit {
			t.Errorf("%s/%s %q: admit=%v, want %v (%v)", check.namespace, check.resource, check.name, got, check.admit, evalErr)
		}
	}
}

func TestUmamiNameReleaseKeepsPriorEvictionAndRejectsUnprovenProgress(t *testing.T) {
	const oldSource = "1111111111111111111111111111111111111111"
	const evictionSource = "2222222222222222222222222222222222222222"
	const workerSource = "3333333333333333333333333333333333333333"
	record := umamiStandbyReplacementRecord{
		Version: 1, Run: "100", Source: oldSource, Phase: umamiReplacementEvictionPending,
		Original:     cnpgStandbyCutover{Consumer: benchmarkResource{Name: "umami-db-1", Namespace: "umami"}},
		Continuation: &umamiReplacementOwner{Run: "200", Source: evictionSource},
	}
	next := record
	next.NameRelease = &umamiStandbyNameRelease{
		Owner: umamiReplacementOwner{Run: "300", Source: workerSource},
		Join:  benchmarkResource{"umami-db-1-join", "umami", freshTestUID},
	}
	next.Phase = umamiClaimDeletePending
	if verifyUmamiReplacementTransition(record, next) == nil {
		t.Fatal("the journal skipped fencing and join retirement")
	}
	next.Phase = umamiNameFencePending
	if err := verifyUmamiReplacementTransition(record, next); err != nil {
		t.Fatal(err)
	}
	record = next
	next.Phase = umamiNameFenced
	if verifyUmamiReplacementTransition(record, next) == nil {
		t.Fatal("the journal accepted fencing without exact receipts")
	}
	policy := *next.NameRelease
	policy.Fence.PolicyUID = "ac005841-c001-4001-8001-000000000001"
	next = record
	next.NameRelease = &policy
	if err := verifyUmamiReplacementTransition(record, next); err != nil {
		t.Fatal("the journal did not checkpoint acceptance of the policy before creating the binding:", err)
	}
	record = next
	fenced := *next.NameRelease
	fenced.Fence = benchmarkFenceReceipt{PolicyUID: "ac005841-c001-4001-8001-000000000001", BindingUID: "ac005841-c002-4002-8002-000000000002"}
	next.NameRelease = &fenced
	next.Phase = umamiNameFenced
	if err := verifyUmamiReplacementTransition(record, next); err != nil {
		t.Fatal(err)
	}
	record = next
	next.Continuation = &umamiReplacementOwner{Run: "201", Source: evictionSource}
	next.Phase = umamiJoinDeletePending
	if verifyUmamiReplacementTransition(record, next) == nil {
		t.Fatal("the journal rewrote the original eviction receipt")
	}
	next = record
	resumed := *next.NameRelease
	resumed.Executor = &umamiReplacementOwner{Run: "301", Source: workerSource}
	next.NameRelease = &resumed
	if err := verifyUmamiReplacementTransition(record, next); err != nil {
		t.Fatal(err)
	}
	resumed.Fence.BindingUID = "ac005841-c003-4003-8003-000000000003"
	if verifyUmamiReplacementTransition(record, next) == nil {
		t.Fatal("a later executor replaced the original fence binding receipt")
	}
}

func TestUmamiNameRecoveryRequiresExactProtectedDispatch(t *testing.T) {
	dispatch := umamiStandbyDispatch()
	dispatch["LONGHORN_V2_OPERATION"] = recoverUmamiStandbyNameOperation
	const dispatchSource = "2222222222222222222222222222222222222222"
	const validConfirm = recoverUmamiStandbyNameOperation + ":umami-db-1:100:" + dispatchSource + ":" + freshTestUID + ":200:" + dispatchSource + ":" + freshTestUID
	dispatch["LONGHORN_V2_CONFIRM"] = validConfirm
	getenv := func(name string) string { return dispatch[name] }
	if _, err := authorizeUmamiStandbyNameRecovery(getenv); err != nil {
		t.Fatal(err)
	}
	dispatch["LONGHORN_V2_CONFIRM"] = validConfirm + ":300"
	if _, err := authorizeUmamiStandbyNameRecovery(getenv); err != nil {
		t.Fatal(err)
	}
	dispatch["GITHUB_RUN_ATTEMPT"] = "2"
	if _, err := authorizeUmamiStandbyNameRecovery(getenv); err == nil {
		t.Fatal("a rerun was allowed to mutate the original Umami claim")
	}
}
