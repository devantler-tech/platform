package nativeproof_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

// Removing the parameter-denial action must fail even when the secondary
// namespace-name condition would not match. This exercises the native order
// of namespace selection, parameter resolution and CEL matching.
func TestNativeParameterResolutionAndNamespaceSelection(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, oldUID string
		labelled, params, deny  bool
		reason                  string
	}{
		{"default off", "customer", "object-owned", false, false, false, ""},
		{"labelled nonbenchmark missing parameters", "customer", "object-owned", true, false, true, "no params found"},
		{"labelled nonbenchmark with parameters", "customer", "object-owned", true, true, false, ""},
		{"labelled benchmark missing parameters", "longhorn-benchmark-v1-1234", "object-owned", true, false, true, "no params found"},
		{"labelled benchmark known UID", "longhorn-benchmark-v1-1234", "object-owned", true, true, false, ""},
		{"labelled benchmark foreign UID", "longhorn-benchmark-v1-1234", "foreign", true, true, true, "Only explicitly recorded benchmark objects may be deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, binding := productionProfile(t)
			// API defaulting supplies an empty object selector.
			policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tc.namespace, UID: "ns-owned"}}
			if tc.labelled {
				namespace.Labels = map[string]string{"storage.platform.devantler.tech/benchmark-delete-guard": "v1"}
			}
			objects := []runtime.Object{namespace}
			if tc.params {
				objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: binding.Spec.ParamRef.Name, Namespace: binding.Spec.ParamRef.Namespace}, Data: map[string]string{"object-owned": "ns-owned"}})
			}
			client := fake.NewClientset(objects...)
			factory := informers.NewSharedInformerFactory(client, 0)
			namespaces := factory.Core().V1().Namespaces()
			params, err := factory.ForResource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"})
			if err != nil {
				t.Fatal(err)
			}
			namespaceCache, parameterCache := namespaces.Informer(), params.Informer()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer func() { cancel(); factory.Shutdown() }()
			factory.Start(ctx.Done())
			if !cache.WaitForCacheSync(ctx.Done(), namespaceCache.HasSynced, parameterCache.HasSynced) {
				t.Fatal("native parameter and namespace fixture caches did not synchronize")
			}
			compiler := admissioncel.NewConditionCompiler(environment.MustBaseEnvSet(version.MajorMinor(1, 36)))
			opts := admissioncel.OptionalVariableDeclarations{HasParams: true}
			validation := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&validating.ValidationCondition{Expression: policy.Spec.Validations[0].Expression, Message: policy.Spec.Validations[0].Message, Reason: policy.Spec.Validations[0].Reason}}, opts, environment.StoredExpressions)
			condition := compiler.CompileCondition([]admissioncel.ExpressionAccessor{&matchconditions.MatchCondition{Name: policy.Spec.MatchConditions[0].Name, Expression: policy.Spec.MatchConditions[0].Expression}}, opts, environment.StoredExpressions)
			if len(validation.CompilationErrors()) != 0 || len(condition.CompilationErrors()) != 0 {
				t.Fatal("native policy compilation failed")
			}
			empty := compiler.CompileCondition(nil, opts, environment.StoredExpressions)
			conditionMatcher := matchconditions.NewMatcher(condition, policy.Spec.FailurePolicy, "policy", "validate", policy.Name)
			validator := validating.NewValidator(validation, conditionMatcher, empty, empty, policy.Spec.FailurePolicy, nil)
			allow := authorizer.AuthorizerFunc(func(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
				return authorizer.DecisionAllow, "fixture", nil
			})
			dispatcher := validating.NewDispatcher(allow, generic.NewPolicyMatcher(matching.NewMatcher(namespaces.Lister(), client)))
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			old := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: tc.namespace, UID: types.UID(tc.oldUID)}}
			attributes := admission.NewAttributesRecord(nil, old, schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, tc.namespace, "", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "", admission.Delete, &metav1.DeleteOptions{}, false, nil)
			err = dispatcher.Dispatch(ctx, attributes, admission.NewObjectInterfacesFromScheme(scheme), []validating.PolicyHook{{Policy: &policy, Bindings: []*validating.PolicyBinding{&binding}, ParamInformer: params, ParamScope: meta.RESTScopeNamespace, Evaluator: validator}})
			if tc.deny {
				var status *apierrors.StatusError
				if !errors.As(err, &status) || status.ErrStatus.Status != metav1.StatusFailure || status.ErrStatus.Code < 400 || status.ErrStatus.Code >= 500 || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("native dispatcher must deny for %q: %v", tc.reason, err)
				}
			} else if err != nil {
				t.Fatalf("native dispatcher unexpectedly denied: %v", err)
			}
		})
	}
}
