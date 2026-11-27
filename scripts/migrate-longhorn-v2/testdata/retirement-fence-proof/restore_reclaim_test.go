package retirementproof_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

func TestNativeRestoreReclaimReferenceDecisions(t *testing.T) {
	raw := os.Getenv("LONGHORN_RESTORE_RECLAIM_FENCE_FIXTURE")
	if raw == "" {
		t.Skip("native restore reclaim fixture is run by its parent test")
	}
	var policies []v1.ValidatingAdmissionPolicy
	if err := json.Unmarshal([]byte(raw), &policies); err != nil || len(policies) != 2 {
		t.Fatalf("complete reclaim policy fixture required: %v", err)
	}
	const workspace = "longhorn-benchmark"
	const pvName = "persistentvolume-native-v2"
	const className = "longhorn-native-v2-5678"
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvName, UID: "restore-PersistentVolume-uid"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "driver.longhorn.io", VolumeHandle: "longhornvolume-native-v2"}},
			ClaimRef:                      &corev1.ObjectReference{Name: "fio-data", Namespace: workspace, UID: "restore-PersistentVolumeClaim-uid"},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, StorageClassName: className,
		}}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "fio-data", Namespace: workspace, UID: "restore-PersistentVolumeClaim-uid"},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvName, StorageClassName: ptrString(className)}}

	changed := pv.DeepCopy()
	changed.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	changed.Status.Phase = corev1.VolumeBound
	finalized := changed.DeepCopy()
	finalized.Finalizers = []string{"external-provisioner.volume.kubernetes.io/finalizer"}
	released := finalized.DeepCopy()
	released.Status.Phase = corev1.VolumeReleased
	aliased := pv.DeepCopy()
	aliased.Name, aliased.UID = "different-backend-reference", ""
	foreign := aliased.DeepCopy()
	foreign.Spec.CSI.VolumeHandle = "different-backend"
	otherPV := foreign.DeepCopy()
	otherPV.Spec.StorageClassName = "different-class"
	rebound := changed.DeepCopy()
	rebound.Spec.ClaimRef.UID = "replacement-claim"
	verifyReclaimDecisions(t, policies[0], "PersistentVolume", "persistentvolumes", "", []decisionCase{
		{name: "allow ordinary reclaim", old: pv, current: changed, operation: admission.Update, selected: true, allowed: true},
		{name: "allow installing CSI finalizer", old: changed, current: finalized, operation: admission.Update, selected: true, allowed: true},
		{name: "allow PV status update", old: finalized, current: released, operation: admission.Update, subresource: "status", selected: true, allowed: true},
		{name: "deny clearing reclaim intent", old: changed, current: pv, operation: admission.Update, selected: true},
		{name: "deny alias PV", current: aliased, operation: admission.Create, selected: true},
		{name: "deny another PV with the same storage class", current: foreign, operation: admission.Create, selected: true},
		{name: "allow unrelated PV", current: otherPV, operation: admission.Create},
		{name: "deny rebind", old: changed, current: rebound, operation: admission.Update, selected: true},
	})

	newClaim := claim.DeepCopy()
	newClaim.UID = ""
	remote := newClaim.DeepCopy()
	remote.Namespace = "different-namespace"
	other := remote.DeepCopy()
	other.Spec.VolumeName, other.Spec.StorageClassName = "different-pv", nil
	claim.Status.Phase = corev1.ClaimPending
	bound := claim.DeepCopy()
	bound.Status.Phase = corev1.ClaimBound
	verifyReclaimDecisions(t, policies[1], "PersistentVolumeClaim", "persistentvolumeclaims", workspace, []decisionCase{
		{name: "allow PVC status update", old: claim, current: bound, operation: admission.Update, subresource: "status", selected: true, allowed: true},
		{name: "deny replacement same name", current: newClaim, operation: admission.Create, selected: true},
		{name: "deny alias in another namespace", resource: remote.Namespace, current: remote, operation: admission.Create, selected: true},
		{name: "ignore unrelated claim", resource: other.Namespace, current: other, operation: admission.Create},
	})
}

func ptrString(value string) *string { return &value }

func verifyReclaimDecisions(t *testing.T, policy v1.ValidatingAdmissionPolicy, kind, resource, namespace string, cases []decisionCase) {
	t.Helper()
	match := policy.Spec.MatchConstraints
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != v1.Fail || match == nil ||
		len(match.ResourceRules) != 1 || len(policy.Spec.Validations) != 1 || len(policy.Spec.MatchConditions) != 1 ||
		len(match.ResourceRules[0].Operations) != 2 || match.ResourceRules[0].Operations[0] != v1.Create ||
		match.ResourceRules[0].Operations[1] != v1.Update || len(match.ResourceRules[0].Resources) != 2 ||
		match.ResourceRules[0].Resources[0] != resource || match.ResourceRules[0].Resources[1] != resource+"/status" {
		t.Fatal("reclaim policy has an unexpected resource or operation scope")
	}
	gvk := schema.GroupVersionKind{Version: "v1", Kind: kind}
	gvr := schema.GroupVersionResource{Version: "v1", Resource: resource}
	for _, mode := range []environment.Type{environment.NewExpressions, environment.StoredExpressions} {
		compiler := admissioncel.NewConditionCompiler(environment.MustBaseEnvSet(version.MajorMinor(1, 36)))
		options := admissioncel.OptionalVariableDeclarations{}
		definition := policy.Spec.Validations[0]
		validation := &validating.ValidationCondition{Expression: definition.Expression, Message: definition.Message, Reason: definition.Reason}
		filter := compiler.CompileCondition([]admissioncel.ExpressionAccessor{validation}, options, mode)
		condition := matchconditions.MatchCondition(policy.Spec.MatchConditions[0])
		conditions := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&condition}, options, mode)
		if len(filter.CompilationErrors())+len(conditions.CompilationErrors()) != 0 {
			t.Fatalf("reclaim policy failed native CEL compilation: %v %v", filter.CompilationErrors(), conditions.CompilationErrors())
		}
		empty := compiler.CompileCondition(nil, options, mode)
		matcher := matchconditions.NewMatcher(conditions, policy.Spec.FailurePolicy, "policy", "validate", policy.Name)
		validator := validating.NewValidator(filter, matcher, empty, empty, policy.Spec.FailurePolicy, nil)
		for _, tc := range cases {
			t.Run(kind+"/"+string(mode)+"/"+tc.name, func(t *testing.T) {
				requestNamespace := namespace
				if tc.resource != "" {
					requestNamespace = tc.resource
				}
				var options runtime.Object = &metav1.UpdateOptions{}
				if tc.operation == admission.Create {
					options = &metav1.CreateOptions{}
				}
				attr := admission.NewAttributesRecord(tc.current, tc.old, gvk, requestNamespace, "", gvr, tc.subresource, tc.operation, options, false, nil)
				versioned := &admission.VersionedAttributes{Attributes: attr, VersionedObject: admission.NewLazyObject(tc.current),
					VersionedOldObject: admission.NewLazyObject(tc.old), VersionedKind: gvk}
				result := validator.Validate(context.Background(), gvr, versioned, nil, nil, celconfig.RuntimeCELCostBudget, nil)
				if !tc.selected {
					if len(result.Decisions) != 0 {
						t.Fatalf("unrelated object was selected: %+v", result)
					}
					return
				}
				if len(result.Decisions) != 1 {
					t.Fatalf("expected one decision: %+v", result)
				}
				decision := result.Decisions[0]
				allowed := decision.Action == validating.ActionAdmit && decision.Evaluation == validating.EvalAdmit
				denied := decision.Action == validating.ActionDeny && decision.Evaluation == validating.EvalDeny
				if tc.allowed && !allowed || !tc.allowed && !denied {
					t.Fatalf("allow=%v want=%v: %+v", allowed, tc.allowed, decision)
				}
			})
		}
	}
}
