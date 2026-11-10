package retirementproof_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	registration "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	plugincel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/mutating/patch"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/cel/environment"
)

const retirementImage = "ghcr.io/devantler-tech/platform-longhorn-instance-manager@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const longhornCreator = "system:serviceaccount:longhorn-system:longhorn-service-account"

func TestNativeRetirementRuntimeAdmission(t *testing.T) {
	raw, err := os.ReadFile("runtime-admission.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy registration.MutatingAdmissionPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Mutations) != 1 || policy.Spec.Mutations[0].JSONPatch == nil || len(policy.Spec.MatchConditions) == 0 {
		t.Fatal("runtime admission policy is incomplete")
	}
	compiler, err := plugincel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		t.Fatal(err)
	}
	expressions := make([]plugincel.ExpressionAccessor, 0, len(policy.Spec.MatchConditions))
	for _, condition := range policy.Spec.MatchConditions {
		item := matchconditions.MatchCondition(condition)
		expressions = append(expressions, &item)
	}
	matcher := matchconditions.NewMatcher(compiler.CompileCondition(expressions, plugincel.OptionalVariableDeclarations{}, environment.StoredExpressions),
		policy.Spec.FailurePolicy, "mutating", "policy", policy.Name)
	mutation := compiler.CompileMutatingEvaluator(&patch.JSONPatchCondition{Expression: policy.Spec.Mutations[0].JSONPatch.Expression},
		plugincel.OptionalVariableDeclarations{HasPatchTypes: true}, environment.StoredExpressions)

	for _, test := range []struct {
		name    string
		edit    func(*unstructured.Unstructured, *user.DefaultInfo)
		matches bool
	}{
		{"native controller", func(*unstructured.Unstructured, *user.DefaultInfo) {}, true},
		{"different creator", func(_ *unstructured.Unstructured, u *user.DefaultInfo) { u.Name = "system:admin" }, false},
		{"different service account", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementPodSpec(p)["serviceAccountName"] = "default"
		}, false},
		{"different namespace", func(p *unstructured.Unstructured, _ *user.DefaultInfo) { p.SetNamespace("other") }, false},
		{"V2", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			labels := p.GetLabels()
			labels["longhorn.io/data-engine"] = "v2"
			p.SetLabels(labels)
		}, false},
		{"no labels", func(p *unstructured.Unstructured, _ *user.DefaultInfo) { p.SetLabels(nil) }, false},
		{"different image", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementPodContainers(p)[0].(map[string]any)["image"] = retirementImage[:len(retirementImage)-1] + "b"
		}, false},
		{"extra container", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementPodSpec(p)["containers"] = append(retirementPodContainers(p), map[string]any{"name": "other"})
		}, false},
		{"unprivileged", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementContainerSecurity(p)["privileged"] = false
		}, false},
		{"explicit container UID", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementContainerSecurity(p)["runAsUser"] = int64(65532)
		}, false},
		{"explicit pod group", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementPodSpec(p)["securityContext"].(map[string]any)["runAsGroup"] = int64(65532)
		}, false},
		{"nonroot required", func(p *unstructured.Unstructured, _ *user.DefaultInfo) {
			retirementPodSpec(p)["securityContext"].(map[string]any)["runAsNonRoot"] = true
		}, false},
		{"unpaired owner", func(p *unstructured.Unstructured, _ *user.DefaultInfo) { p.SetName("instance-manager-other") }, false},
		{"missing owner", func(p *unstructured.Unstructured, _ *user.DefaultInfo) { p.SetOwnerReferences(nil) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := retirementRuntimePod()
			creator := &user.DefaultInfo{Name: longhornCreator}
			test.edit(pod, creator)
			gvk := schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
			gvr := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
			a := admission.NewAttributesRecord(pod, nil, gvk, pod.GetNamespace(), pod.GetName(), gvr, "", admission.Create, &metav1.CreateOptions{}, false, creator)
			v := &admission.VersionedAttributes{Attributes: a, VersionedKind: gvk, VersionedObject: pod}
			result := matcher.Match(context.Background(), v, nil, nil)
			if result.Error != nil || result.Matches != test.matches {
				t.Fatalf("match = %+v, want %v", result, test.matches)
			}
			if !result.Matches {
				return
			}
			out, err := patch.NewJSONPatcher(mutation).Patch(compiler.CreateContext(context.Background()),
				patch.Request{MatchedResource: gvr, VersionedAttributes: v, ObjectInterfaces: admission.NewObjectInterfacesFromScheme(runtime.NewScheme())},
				celconfig.RuntimeCELCostBudget)
			if err != nil {
				t.Fatal(err)
			}
			security := retirementContainerSecurity(out.(*unstructured.Unstructured))
			if security["runAsUser"] != int64(0) || security["runAsGroup"] != int64(0) || security["privileged"] != true || security["seLinuxOptions"] == nil {
				t.Fatalf("unexpected security context: %+v", security)
			}
		})
	}
}

func retirementRuntimePod() *unstructured.Unstructured {
	const name = "instance-manager-11111111111111111111111111111111"
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": "longhorn-system",
			"labels": map[string]any{
				"longhorn.io/component": "instance-manager", "longhorn.io/data-engine": "v1",
				"longhorn.io/instance-manager-type": "aio", "longhorn.io/managed-by": "longhorn-manager",
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "name": name,
				"uid": "11111111-1111-4111-8111-111111111111", "controller": true,
			}},
		},
		"spec": map[string]any{
			"serviceAccountName": "longhorn-service-account", "restartPolicy": "Never",
			"securityContext": map[string]any{"fsGroupChangePolicy": "OnRootMismatch"},
			"containers": []any{map[string]any{"name": "instance-manager", "image": retirementImage,
				"securityContext": map[string]any{"privileged": true, "seLinuxOptions": map[string]any{}}}},
		},
	}}
}

func retirementPodSpec(pod *unstructured.Unstructured) map[string]any {
	return pod.Object["spec"].(map[string]any)
}

func retirementPodContainers(pod *unstructured.Unstructured) []any {
	return retirementPodSpec(pod)["containers"].([]any)
}

func retirementContainerSecurity(pod *unstructured.Unstructured) map[string]any {
	return retirementPodContainers(pod)[0].(map[string]any)["securityContext"].(map[string]any)
}
