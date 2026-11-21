package nativeproof_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

func productionProfile(t *testing.T) (v1.ValidatingAdmissionPolicy, v1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("LONGHORN_GUARD_RENDER_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	var policy v1.ValidatingAdmissionPolicy
	var binding v1.ValidatingAdmissionPolicyBinding
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var document json.RawMessage
		if err := decoder.Decode(&document); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var identity struct {
			Kind     string                `json:"kind"`
			Metadata struct{ Name string } `json:"metadata"`
		}
		if json.Unmarshal(document, &identity) != nil {
			t.Fatal("invalid controller document")
		}
		if identity.Metadata.Name != "longhorn-benchmark-delete" {
			continue
		}
		switch identity.Kind {
		case "ValidatingAdmissionPolicy":
			if policy.Name != "" || json.Unmarshal(document, &policy) != nil {
				t.Fatal("ambiguous production deletion policy")
			}
		case "ValidatingAdmissionPolicyBinding":
			if binding.Name != "" || json.Unmarshal(document, &binding) != nil {
				t.Fatal("ambiguous production deletion binding")
			}
		}
	}
	if policy.Name == "" || binding.Name == "" {
		t.Fatal("benchmark deletion policy and binding must be rendered in production")
	}
	return policy, binding
}

func TestProductionSelectionAndDenyBehavior(t *testing.T) {
	policy, binding := productionProfile(t)
	if policy.APIVersion != "admissionregistration.k8s.io/v1" || binding.APIVersion != policy.APIVersion || policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != v1.Fail {
		t.Fatal("native deletion failures must deny")
	}
	match := policy.Spec.MatchConstraints
	if match == nil || match.MatchPolicy == nil || *match.MatchPolicy != v1.Exact || len(match.ExcludeResourceRules) != 0 || len(match.ResourceRules) != 1 {
		t.Fatal("deletion coverage must have one complete namespaced rule")
	}
	rule := match.ResourceRules[0]
	if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "*" || len(rule.APIVersions) != 1 || rule.APIVersions[0] != "*" || len(rule.Resources) != 1 || rule.Resources[0] != "*" || len(rule.Operations) != 1 || rule.Operations[0] != v1.Delete || rule.Scope == nil || *rule.Scope != v1.NamespacedScope || len(rule.ResourceNames) != 0 || match.ObjectSelector != nil {
		t.Fatal("every namespaced parent DELETE must be covered without object-label escape")
	}
	selector, err := metav1.LabelSelectorAsSelector(match.NamespaceSelector)
	if err != nil || selector.Matches(labels.Set{}) || !selector.Matches(labels.Set{"storage.platform.devantler.tech/benchmark-delete-guard": "v1"}) || selector.Matches(labels.Set{"storage.platform.devantler.tech/benchmark-delete-guard": "disabled"}) {
		t.Fatal("namespace guard must be explicitly enabled and default off")
	}
	if policy.Spec.ParamKind == nil || policy.Spec.ParamKind.APIVersion != "v1" || policy.Spec.ParamKind.Kind != "ConfigMap" || binding.Spec.PolicyName != policy.Name || binding.Spec.ParamRef == nil || binding.Spec.ParamRef.Name != "longhorn-benchmark-delete-allowlist" || binding.Spec.ParamRef.Namespace != "longhorn-system" || binding.Spec.ParamRef.Selector != nil || binding.Spec.ParamRef.ParameterNotFoundAction == nil || *binding.Spec.ParamRef.ParameterNotFoundAction != v1.DenyAction || len(binding.Spec.ValidationActions) != 1 || binding.Spec.ValidationActions[0] != v1.Deny || binding.Spec.MatchResources != nil {
		t.Fatal("one fixed parameter object and missing-parameter denial are required")
	}
}

func TestNativeUIDBoundDeletion(t *testing.T) {
	policy, _ := productionProfile(t)
	if len(policy.Spec.Validations) != 1 || len(policy.Spec.MatchConditions) != 1 {
		t.Fatal("audit changed policy conditions before extending the proof")
	}
	for _, mode := range []environment.Type{environment.NewExpressions, environment.StoredExpressions} {
		t.Run(string(mode), func(t *testing.T) {
			compiler := admissioncel.NewConditionCompiler(environment.MustBaseEnvSet(version.MajorMinor(1, 36)))
			opts := admissioncel.OptionalVariableDeclarations{HasParams: true}
			filter := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&validating.ValidationCondition{Expression: policy.Spec.Validations[0].Expression}}, opts, mode)
			condition := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&validating.ValidationCondition{Expression: policy.Spec.MatchConditions[0].Expression}}, opts, mode)
			if len(filter.CompilationErrors()) != 0 || len(condition.CompilationErrors()) != 0 {
				t.Fatalf("native policy compilation failed: %v %v", filter.CompilationErrors(), condition.CompilationErrors())
			}
			empty := compiler.CompileCondition(nil, opts, mode)
			validator := validating.NewValidator(filter, nil, empty, empty, policy.Spec.FailurePolicy, nil)
			gvk := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
			gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
			old := func(uid any) runtime.Object {
				return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "owned", "namespace": "longhorn-benchmark-v1-1234", "uid": uid}}}
			}
			namespace := func(uid string) *corev1.Namespace {
				return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "longhorn-benchmark-v1-1234", UID: types.UID(uid)}}
			}
			params := func() runtime.Object {
				return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "longhorn-benchmark-delete-allowlist", Namespace: "longhorn-system"}, Data: map[string]string{"object-owned": "ns-owned"}}
			}
			for _, tc := range []struct {
				name      string
				old       runtime.Object
				params    runtime.Object
				namespace *corev1.Namespace
				allow     bool
			}{
				{"exact UID pair and empty collection name", old("object-owned"), params(), namespace("ns-owned"), true},
				{"unknown object UID", old("foreign"), params(), namespace("ns-owned"), false},
				{"replaced namespace", old("object-owned"), params(), namespace("replacement"), false},
				{"missing params", old("object-owned"), nil, namespace("ns-owned"), false},
				{"missing namespace", old("object-owned"), params(), nil, false},
				{"empty namespace UID", old("object-owned"), params(), namespace(""), false},
				{"missing old object", nil, params(), namespace("ns-owned"), false},
				{"empty old UID", old(""), params(), namespace("ns-owned"), false},
				{"numeric old UID", old(int64(42)), params(), namespace("ns-owned"), false},
				{"missing old metadata", &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}, params(), namespace("ns-owned"), false},
				{"missing parameter data", old("object-owned"), &corev1.ConfigMap{}, namespace("ns-owned"), false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					attr := admission.NewAttributesRecord(nil, tc.old, gvk, "longhorn-benchmark-v1-1234", "", gvr, "", admission.Delete, &metav1.DeleteOptions{}, false, nil)
					versioned := &admission.VersionedAttributes{Attributes: attr, VersionedOldObject: admission.NewLazyObject(tc.old), VersionedKind: gvk}
					result := validator.Validate(context.Background(), gvr, versioned, tc.params, tc.namespace, celconfig.RuntimeCELCostBudget, nil)
					if len(result.Decisions) != 1 {
						t.Fatalf("native decision missing: %+v", result)
					}
					decision := result.Decisions[0]
					allowed := decision.Action == validating.ActionAdmit && decision.Evaluation == validating.EvalAdmit
					if allowed != tc.allow {
						t.Fatalf("allow=%v want=%v native decision=%+v", allowed, tc.allow, decision)
					}
				})
			}
			for _, tc := range []struct {
				namespace string
				selected  bool
			}{
				{"longhorn-benchmark-v1-1234", true}, {"longhorn-benchmark-v2-999", true},
				{"longhorn-system", false}, {"customer", false}, {"longhorn-benchmark-v1-0", false},
			} {
				attr := admission.NewAttributesRecord(nil, old("object-owned"), gvk, tc.namespace, "", gvr, "", admission.Delete, &metav1.DeleteOptions{}, false, nil)
				versioned := &admission.VersionedAttributes{Attributes: attr, VersionedOldObject: admission.NewLazyObject(old("object-owned")), VersionedKind: gvk}
				request := admissioncel.CreateAdmissionRequest(attr, metav1.GroupVersionResource(gvr), metav1.GroupVersionKind(gvk))
				result, _, err := condition.ForInput(context.Background(), versioned, request, admissioncel.OptionalVariableBindings{VersionedParams: params()}, namespace("ns-owned"), celconfig.RuntimeCELCostBudget)
				if err != nil || len(result) != 1 || result[0].Error != nil || result[0].EvalResult.Value() != tc.selected {
					t.Fatalf("native namespace condition=%s selected=%v result=%+v err=%v", tc.namespace, tc.selected, result, err)
				}
			}
		})
	}
}
