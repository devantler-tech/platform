package gatewayreceiver_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	registration "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating/patch"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"
	"strings"
)

var gvk = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumEnvoyConfig"}
var gvr = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumenvoyconfigs"}

func policyFile(t *testing.T, name string, out any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s/providers/hetzner/infrastructure/cilium/gateway-receiver", name))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "${gateway_receiver_source_cidr}", "192.0.2.5/32"))
	if err = yaml.UnmarshalStrict(raw, out); err != nil {
		t.Fatal(err)
	}
}
func compiler(t *testing.T, vars []registration.Variable) *plugincel.CompositedCompiler {
	t.Helper()
	c, err := plugincel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		t.Fatal(err)
	}
	named := make([]plugincel.NamedExpressionAccessor, 0, len(vars))
	for _, v := range vars {
		named = append(named, &validating.Variable{Name: v.Name, Expression: v.Expression})
	}
	c.CompileAndStoreVariables(named, plugincel.OptionalVariableDeclarations{}, environment.StoredExpressions)
	return c
}
func attrs(obj *unstructured.Unstructured) *admission.VersionedAttributes {
	a := admission.NewAttributesRecord(obj, nil, gvk, obj.GetNamespace(), obj.GetName(), gvr, "", admission.Create, &metav1.CreateOptions{}, false, nil)
	return &admission.VersionedAttributes{Attributes: a, VersionedKind: gvk, VersionedObject: obj}
}
func mutate(t *testing.T, p registration.MutatingAdmissionPolicy, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	c := compiler(t, p.Spec.Variables)
	ctx := c.CreateContext(context.Background())
	v := attrs(obj.DeepCopy())
	for _, m := range p.Spec.Mutations {
		if m.PatchType != registration.PatchTypeJSONPatch || m.JSONPatch == nil {
			t.Fatal("unsupported mutation")
		}
		e := c.CompileMutatingEvaluator(&patch.JSONPatchCondition{Expression: m.JSONPatch.Expression}, plugincel.OptionalVariableDeclarations{HasPatchTypes: true}, environment.StoredExpressions)
		out, err := patch.NewJSONPatcher(e).Patch(ctx, patch.Request{MatchedResource: gvr, VersionedAttributes: v, ObjectInterfaces: admission.NewObjectInterfacesFromScheme(runtime.NewScheme()), OptionalVariables: plugincel.OptionalVariableBindings{}}, celconfig.RuntimeCELCostBudget)
		if err != nil {
			t.Fatal(err)
		}
		v.VersionedObject = out
	}
	return v.VersionedObject.(*unstructured.Unstructured)
}
func validates(t *testing.T, p registration.ValidatingAdmissionPolicy, obj *unstructured.Unstructured) bool {
	t.Helper()
	c := compiler(t, p.Spec.Variables)
	checks := make([]plugincel.ExpressionAccessor, 0, len(p.Spec.Validations))
	for _, v := range p.Spec.Validations {
		checks = append(checks, &validating.ValidationCondition{Expression: v.Expression})
	}
	e := c.CompileCondition(checks, plugincel.OptionalVariableDeclarations{}, environment.StoredExpressions)
	v := attrs(obj)
	req := plugincel.CreateAdmissionRequest(v.Attributes, metav1.GroupVersionResource(gvr), metav1.GroupVersionKind(gvk))
	result, _, err := e.ForInput(c.CreateContext(context.Background()), v, (*admissionv1.AdmissionRequest)(req), plugincel.OptionalVariableBindings{}, nil, celconfig.RuntimeCELCostBudget)
	if err != nil {
		return false
	}
	for _, r := range result {
		if r.Error != nil || r.EvalResult.Value() != true {
			return false
		}
	}
	return len(result) == len(checks) && len(result) > 0
}
func fixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumEnvoyConfig", "metadata": map[string]any{"name": "cilium-gateway-platform", "namespace": "kube-system"}, "spec": map[string]any{"resources": []any{
		map[string]any{"@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster", "name": "unchanged"},
		map[string]any{"@type": "type.googleapis.com/envoy.config.listener.v3.Listener", "name": "listener", "filterChains": []any{
			map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "raw_buffer"}, "filters": []any{}},
			map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "tls", "serverNames": []any{"example.invalid"}}, "filters": []any{}},
			map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "tls"}, "filters": []any{}},
		}},
	}}}}
}
func listener(obj *unstructured.Unstructured) map[string]any {
	return obj.Object["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
}
func TestNativeAdmissionPreservesUnrelatedResourcesAndEveryTLSChain(t *testing.T) {
	var m registration.MutatingAdmissionPolicy
	var v registration.ValidatingAdmissionPolicy
	policyFile(t, "mutating-admission-policy-gateway-receiver.yaml", &m)
	policyFile(t, "validating-admission-policy-gateway-receiver.yaml", &v)
	if m.Spec.FailurePolicy == nil || *m.Spec.FailurePolicy != registration.Fail || v.Spec.FailurePolicy == nil || *v.Spec.FailurePolicy != registration.Fail {
		t.Fatal("admission must fail closed")
	}
	before := fixture()
	after := mutate(t, m, before)
	if !validates(t, v, after) {
		t.Fatal("native validation refused the actual native mutation")
	}
	b := listener(before)["filterChains"].([]any)
	a := listener(after)["filterChains"].([]any)
	if !reflect.DeepEqual(b[0], a[0]) {
		t.Fatal("HTTP diagnostic changed")
	}
	for _, chain := range a[1:] {
		match := chain.(map[string]any)["filterChainMatch"].(map[string]any)
		want := []any{map[string]any{"addressPrefix": "192.0.2.5", "prefixLen": int64(32)}}
		if !reflect.DeepEqual(match["directSourcePrefixRanges"], want) {
			t.Fatalf("physical peer match=%v", match["directSourcePrefixRanges"])
		}
	}
	if !reflect.DeepEqual(after, mutate(t, m, after)) {
		t.Fatal("mutation is not idempotent")
	}
	for _, bad := range []string{"absent", "broader", "wrong-peer", "fallback", "matcher", "fallback-alias", "matcher-alias", "socket-alias", "peer-alias", "ambiguous-listener", "missing-transport", "marker"} {
		t.Run(bad, func(t *testing.T) {
			obj := after.DeepCopy()
			l := listener(obj)
			chains := l["filterChains"].([]any)
			match := chains[1].(map[string]any)["filterChainMatch"].(map[string]any)
			switch bad {
			case "absent":
				delete(match, "directSourcePrefixRanges")
			case "broader":
				match["directSourcePrefixRanges"].([]any)[0].(map[string]any)["prefixLen"] = int64(24)
			case "wrong-peer":
				match["directSourcePrefixRanges"].([]any)[0].(map[string]any)["addressPrefix"] = "192.0.2.6"
			case "fallback":
				l["defaultFilterChain"] = map[string]any{"filters": []any{}}
			case "matcher":
				l["filterChainMatcher"] = map[string]any{}
			case "fallback-alias":
				l["default_filter_chain"] = map[string]any{"filters": []any{}}
			case "matcher-alias":
				l["filter_chain_matcher"] = map[string]any{}
			case "socket-alias":
				chains[0].(map[string]any)["transport_socket"] = map[string]any{}
			case "peer-alias":
				match["direct_source_prefix_ranges"] = []any{map[string]any{"address_prefix": "0.0.0.0", "prefix_len": int64(0)}}
			case "ambiguous-listener":
				r := obj.Object["spec"].(map[string]any)["resources"].([]any)
				obj.Object["spec"].(map[string]any)["resources"] = append(r, l)
			case "missing-transport":
				delete(match, "transportProtocol")
			case "marker":
				delete(obj.Object["metadata"].(map[string]any)["annotations"].(map[string]any), "platform.devantler.tech/gateway-receiver-owner")
			}
			if validates(t, v, obj) {
				t.Fatal("unsafe or ambiguous peer match admitted")
			}
		})
	}
}

func TestNativeMatchConditionsSelectOnlyTheDeclaredGateway(t *testing.T) {
	var m registration.MutatingAdmissionPolicy
	var v registration.ValidatingAdmissionPolicy
	policyFile(t, "mutating-admission-policy-gateway-receiver.yaml", &m)
	policyFile(t, "validating-admission-policy-gateway-receiver.yaml", &v)
	exact, scope := registration.Exact, registration.NamespacedScope
	expected := &registration.MatchResources{MatchPolicy: &exact, NamespaceSelector: &metav1.LabelSelector{}, ObjectSelector: &metav1.LabelSelector{}, ResourceRules: []registration.NamedRuleWithOperations{{RuleWithOperations: registration.RuleWithOperations{Operations: []registration.OperationType{registration.Create, registration.Update}, Rule: registration.Rule{APIGroups: []string{"cilium.io"}, APIVersions: []string{"v2"}, Resources: []string{"ciliumenvoyconfigs"}, Scope: &scope}}}}}
	for _, p := range []struct {
		kind        string
		constraints *registration.MatchResources
		conditions  []registration.MatchCondition
	}{
		{"MutatingAdmissionPolicy", m.Spec.MatchConstraints, m.Spec.MatchConditions},
		{"ValidatingAdmissionPolicy", v.Spec.MatchConstraints, v.Spec.MatchConditions},
	} {
		t.Run(p.kind, func(t *testing.T) {
			if !reflect.DeepEqual(p.constraints, expected) {
				t.Fatal("policy selects another resource, operation, version or scope")
			}
			if len(p.conditions) != 1 {
				t.Fatal("missing gateway match condition")
			}
			c := compiler(t, nil)
			expressions := make([]plugincel.ExpressionAccessor, 0, len(p.conditions))
			for _, condition := range p.conditions {
				item := matchconditions.MatchCondition(condition)
				expressions = append(expressions, &item)
			}
			evaluator := c.CompileCondition(expressions, plugincel.OptionalVariableDeclarations{}, environment.StoredExpressions)
			fail := registration.Fail
			matcher := matchconditions.NewMatcher(evaluator, &fail, p.kind, "policy", "gateway-receiver")
			for _, test := range []struct {
				namespace, name string
				want            bool
			}{
				{"kube-system", "cilium-gateway-platform", true},
				{"another", "cilium-gateway-platform", false},
				{"kube-system", "another", false},
				{"another", "another", false},
			} {
				obj := fixture()
				obj.SetNamespace(test.namespace)
				obj.SetName(test.name)
				result := matcher.Match(context.Background(), attrs(obj), nil, nil)
				if result.Error != nil || result.Matches != test.want {
					t.Fatal("native matcher selected another gateway or refused its declared gateway")
				}
			}
		})
	}
}
