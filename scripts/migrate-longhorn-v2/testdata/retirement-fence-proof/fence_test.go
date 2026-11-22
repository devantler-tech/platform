package retirementproof_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

type decisionCase struct {
	name              string
	resource          string
	old, current      runtime.Object
	operation         admission.Operation
	subresource       string
	selected, allowed bool
}

func TestNativeOriginalReferenceFences(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("LONGHORN_RETIREMENT_FENCE_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	var byPhase map[string][]v1.ValidatingAdmissionPolicy
	if json.Unmarshal(raw, &byPhase) != nil || len(byPhase) != 2 {
		t.Fatal("complete generated policies are required")
	}
	for _, phase := range []string{"v1", "v2"} {
		policies := byPhase[phase]
		if len(policies) != 3 {
			t.Fatal("all three unbound policies are required")
		}
		for _, policy := range policies {
			match := policy.Spec.MatchConstraints
			if policy.Kind != "ValidatingAdmissionPolicy" || policy.Spec.ParamKind != nil || policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != v1.Fail || match == nil || match.MatchPolicy == nil || *match.MatchPolicy != v1.Equivalent || match.ObjectSelector == nil || match.NamespaceSelector == nil || len(match.ObjectSelector.MatchLabels)+len(match.ObjectSelector.MatchExpressions)+len(match.NamespaceSelector.MatchLabels)+len(match.NamespaceSelector.MatchExpressions) != 0 || len(match.ExcludeResourceRules) != 0 || len(match.ResourceRules) != 1 || len(policy.Spec.Validations) != 1 || len(policy.Spec.MatchConditions) != 1 {
				t.Fatal("fixed scope, failure denial and label-independent selection are required")
			}
			isPV := strings.Contains(policy.Name, "retire-pv")
			isInstance := strings.Contains(policy.Name, "retire-instances")
			gvk := schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Volume"}
			gvr := schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}
			namespace := "longhorn-system"
			cases := backendCases(phase)
			if isPV {
				gvk, gvr, namespace = schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolume"}, schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}, ""
				cases = pvCases(phase)
			}
			if isInstance {
				gvk.Kind, gvr.Resource = "Engine", "engines"
				cases = instanceCases(phase)
			}
			for _, mode := range []environment.Type{environment.NewExpressions, environment.StoredExpressions} {
				t.Run(phase+"/"+policy.Name+"/"+string(mode), func(t *testing.T) {
					compiler := admissioncel.NewConditionCompiler(environment.MustBaseEnvSet(version.MajorMinor(1, 36)))
					opts := admissioncel.OptionalVariableDeclarations{}
					definition := policy.Spec.Validations[0]
					filter := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&validating.ValidationCondition{Expression: definition.Expression, Message: definition.Message, Reason: definition.Reason}}, opts, mode)
					condition := matchconditions.MatchCondition(policy.Spec.MatchConditions[0])
					conditions := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&condition}, opts, mode)
					if len(filter.CompilationErrors())+len(conditions.CompilationErrors()) != 0 {
						t.Fatalf("native compilation failed: %v %v", filter.CompilationErrors(), conditions.CompilationErrors())
					}
					empty := compiler.CompileCondition(nil, opts, mode)
					matcher := matchconditions.NewMatcher(conditions, policy.Spec.FailurePolicy, "policy", "validate", policy.Name)
					validator := validating.NewValidator(filter, matcher, empty, empty, policy.Spec.FailurePolicy, nil)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "longhorn-system", UID: "fixture-namespace"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "fixture-default"}})
					factory := informers.NewSharedInformerFactory(client, 0)
					namespaces := factory.Core().V1().Namespaces()
					namespaceCache := namespaces.Informer()
					factory.Start(ctx.Done())
					defer factory.Shutdown()
					defer cancel()
					if !cache.WaitForCacheSync(ctx.Done(), namespaceCache.HasSynced) {
						t.Fatal("native namespace fixture did not synchronize")
					}
					allow := authorizer.AuthorizerFunc(func(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
						return authorizer.DecisionAllow, "fixture", nil
					})
					dispatcher := validating.NewDispatcher(allow, generic.NewPolicyMatcher(matching.NewMatcher(namespaces.Lister(), client)))
					scheme := runtime.NewScheme()
					if err := corev1.AddToScheme(scheme); err != nil {
						t.Fatal(err)
					}
					if !isPV {
						scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
					}
					if isInstance {
						for _, kind := range []string{"Replica", "EngineFrontend", "VolumeAttachment"} {
							scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: kind}, &unstructured.Unstructured{})
						}
					}
					binding := &v1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: policy.Name}, Spec: v1.ValidatingAdmissionPolicyBindingSpec{PolicyName: policy.Name, ValidationActions: []v1.ValidationAction{v1.Deny}}}
					hooks := []validating.PolicyHook{{Policy: &policy, Bindings: []*validating.PolicyBinding{binding}, Evaluator: validator}}
					for _, tc := range cases {
						t.Run(tc.name, func(t *testing.T) {
							if strings.Contains(tc.name, "native finalizer removal") {
								before, oldErr := apimeta.Accessor(tc.old)
								after, newErr := apimeta.Accessor(tc.current)
								if oldErr != nil || newErr != nil || len(before.GetFinalizers()) == 0 || len(after.GetFinalizers()) != 0 {
									t.Fatal("finalizer-removal probe must exercise an actual removal")
								}
							}
							caseGVK, caseGVR := gvk, gvr
							if tc.resource != "" {
								caseGVR.Resource = tc.resource
								caseGVK.Kind = map[string]string{"engines": "Engine", "replicas": "Replica", "enginefrontends": "EngineFrontend", "volumeattachments": "VolumeAttachment"}[tc.resource]
							}
							attr := admission.NewAttributesRecord(tc.current, tc.old, caseGVK, namespace, "", caseGVR, tc.subresource, tc.operation, &metav1.UpdateOptions{}, false, nil)
							versioned := &admission.VersionedAttributes{Attributes: attr, VersionedObject: admission.NewLazyObject(tc.current), VersionedOldObject: admission.NewLazyObject(tc.old), VersionedKind: caseGVK}
							result := validator.Validate(context.Background(), caseGVR, versioned, nil, nil, celconfig.RuntimeCELCostBudget, nil)
							if !tc.selected {
								if len(result.Decisions) != 0 {
									t.Fatalf("unrelated object was selected: %+v", result)
								}
								if err := dispatcher.Dispatch(ctx, attr, admission.NewObjectInterfacesFromScheme(scheme), hooks); err != nil {
									t.Fatalf("native dispatcher selected an unrelated reference: %v", err)
								}
								return
							}
							if len(result.Decisions) != 1 {
								t.Fatalf("native decision missing: %+v", result)
							}
							decision := result.Decisions[0]
							allowed := decision.Action == validating.ActionAdmit && decision.Evaluation == validating.EvalAdmit
							if allowed != tc.allowed {
								t.Fatalf("allow=%v want=%v decision=%+v", allowed, tc.allowed, decision)
							}
							dispatchErr := dispatcher.Dispatch(ctx, attr, admission.NewObjectInterfacesFromScheme(scheme), hooks)
							if tc.allowed && dispatchErr != nil {
								t.Fatalf("native dispatcher refused controller update: %v", dispatchErr)
							}
							if !tc.allowed {
								var status *apierrors.StatusError
								if !errors.As(dispatchErr, &status) || status.ErrStatus.Code != 403 || status.ErrStatus.Reason != metav1.StatusReasonForbidden {
									t.Fatalf("native dispatcher failed to deny: %v", dispatchErr)
								}
								if status.ErrStatus.Details == nil || len(status.ErrStatus.Details.Causes) != 1 || status.ErrStatus.Details.Causes[0].Message != "ValidatingAdmissionPolicy '"+policy.Name+"' with binding '"+binding.Name+"' denied request: "+policy.Spec.Validations[0].Message {
									t.Fatalf("native denial was not attributed to the exact guard: %v", dispatchErr)
								}
							}
						})
					}
					t.Run("resource and operation selection", func(t *testing.T) {
						// Deletes remain governed by original-UID retirement gates,
						// rather than being stranded by a CREATE/UPDATE invariant.
						old := cases[0].old
						deletion := admission.NewAttributesRecord(nil, old, gvk, namespace, "", gvr, "", admission.Delete, &metav1.DeleteOptions{}, false, nil)
						if err := dispatcher.Dispatch(ctx, deletion, admission.NewObjectInterfacesFromScheme(scheme), hooks); err != nil {
							t.Fatalf("ordinary DELETE was selected: %v", err)
						}
						pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "scratch-pv", Namespace: "default"}}
						crossResource := admission.NewAttributesRecord(pod, nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, "default", "scratch-pv", schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "", admission.Create, &metav1.CreateOptions{}, false, nil)
						if err := dispatcher.Dispatch(ctx, crossResource, admission.NewObjectInterfacesFromScheme(scheme), hooks); err != nil {
							t.Fatalf("same-name unrelated kind was selected: %v", err)
						}
					})
				})
			}
		}
	}
}

func pvCases(phase string) []decisionCase {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "scratch-pv", UID: types.UID("uid-scratch-pv"), Finalizers: []string{"kubernetes.io/pv-protection"}}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "driver.longhorn.io", VolumeHandle: "scratch-backend"}}, ClaimRef: &corev1.ObjectReference{UID: types.UID("scratch-claim"), Name: "fio-data", Namespace: "longhorn-benchmark-" + phase + "-1234"}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, StorageClassName: "lh-benchmark-" + phase + "-1234"}}
	cases := []decisionCase{}
	add := func(name string, mutate func(*corev1.PersistentVolume), operation admission.Operation, selected, allowed bool) {
		current := pv.DeepCopy()
		mutate(current)
		var old runtime.Object = pv.DeepCopy()
		if operation == admission.Create {
			old = nil
		}
		cases = append(cases, decisionCase{name: name, old: old, current: current, operation: operation, selected: selected, allowed: allowed})
	}
	add("native finalizer removal", func(p *corev1.PersistentVolume) { p.Finalizers = nil }, admission.Update, true, true)
	add("native Released status", func(p *corev1.PersistentVolume) { p.Status.Phase = corev1.VolumeReleased }, admission.Update, true, true)
	cases[len(cases)-1].subresource = "status"
	add("static alias CREATE", func(p *corev1.PersistentVolume) { p.Name = "alias"; p.UID = "" }, admission.Create, true, false)
	add("original name foreign handle CREATE", func(p *corev1.PersistentVolume) { p.Spec.CSI.VolumeHandle = "foreign"; p.UID = "" }, admission.Create, true, false)
	add("replacement UID", func(p *corev1.PersistentVolume) { p.UID = "replacement" }, admission.Update, true, false)
	add("claim rebound", func(p *corev1.PersistentVolume) { p.Spec.ClaimRef.UID = "foreign" }, admission.Update, true, false)
	add("handle escaped", func(p *corev1.PersistentVolume) { p.Spec.CSI.VolumeHandle = "foreign" }, admission.Update, true, false)
	add("Delete reclaim", func(p *corev1.PersistentVolume) {
		p.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	}, admission.Update, true, false)
	add("unrelated CREATE", func(p *corev1.PersistentVolume) { p.Name = "foreign"; p.Spec.CSI.VolumeHandle = "foreign"; p.UID = "" }, admission.Create, false, false)
	return cases
}

func backendCases(phase string) []decisionCase {
	volume := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "metadata": map[string]any{"name": "scratch-backend", "namespace": "longhorn-system", "uid": "uid-scratch-backend", "finalizers": []any{"longhorn.io"}}, "spec": map[string]any{"dataEngine": phase}, "status": map[string]any{"state": "detached", "kubernetesStatus": map[string]any{}}}}
	cases := []decisionCase{}
	add := func(name string, mutate func(*unstructured.Unstructured), operation admission.Operation, selected, allowed bool) {
		current := volume.DeepCopy()
		mutate(current)
		var old runtime.Object = volume.DeepCopy()
		if operation == admission.Create {
			old = nil
		}
		cases = append(cases, decisionCase{name: name, old: old, current: current, operation: operation, selected: selected, allowed: allowed})
	}
	add("native finalizer removal", func(v *unstructured.Unstructured) { v.SetFinalizers(nil) }, admission.Update, true, true)
	add("native deleting status", func(v *unstructured.Unstructured) { v.Object["status"].(map[string]any)["state"] = "deleting" }, admission.Update, true, true)
	cases[len(cases)-1].subresource = "status"
	add("recreated backend", func(v *unstructured.Unstructured) { v.SetUID("") }, admission.Create, true, false)
	add("request namespace cannot be escaped in metadata", func(v *unstructured.Unstructured) { v.SetNamespace(""); v.SetUID("") }, admission.Create, true, false)
	add("foreign metadata namespace cannot escape request scope", func(v *unstructured.Unstructured) { v.SetNamespace("foreign"); v.SetUID("") }, admission.Create, true, false)
	add("replacement backend", func(v *unstructured.Unstructured) { v.SetUID("replacement") }, admission.Update, true, false)
	add("PV name repopulated", func(v *unstructured.Unstructured) {
		v.Object["status"].(map[string]any)["kubernetesStatus"].(map[string]any)["pvName"] = "scratch-pv"
	}, admission.Update, true, false)
	cases[len(cases)-1].subresource = "status"
	add("PVC name repopulated", func(v *unstructured.Unstructured) {
		v.Object["status"].(map[string]any)["kubernetesStatus"].(map[string]any)["pvcName"] = "fio-data"
	}, admission.Update, true, false)
	add("retired PVC history", func(v *unstructured.Unstructured) {
		v.Object["status"].(map[string]any)["kubernetesStatus"] = map[string]any{"pvcName": "fio-data", "lastPVCRefAt": "2026-11-02T14:00:00Z"}
	}, admission.Update, true, true)
	add("desired attach", func(v *unstructured.Unstructured) { v.Object["spec"].(map[string]any)["nodeID"] = "foreign-node" }, admission.Update, true, false)
	add("observed attach", func(v *unstructured.Unstructured) {
		v.Object["status"].(map[string]any)["currentNodeID"] = "foreign-node"
	}, admission.Update, true, false)
	add("unrelated backend", func(v *unstructured.Unstructured) { v.SetName("foreign") }, admission.Create, false, false)
	return cases
}
