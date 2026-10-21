// This offline test driver uses the deployed Kyverno compiler and admission
// engine. Unlike kyverno apply/test 1.19.1, it preserves distinct old/new
// objects and an explicit subresource. It never connects to Kubernetes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	celengine "github.com/kyverno/kyverno/pkg/cel/engine"
	"github.com/kyverno/kyverno/pkg/cel/libs"
	"github.com/kyverno/kyverno/pkg/cel/matching"
	"github.com/kyverno/kyverno/pkg/cel/policies/vpol/compiler"
	vpolengine "github.com/kyverno/kyverno/pkg/cel/policies/vpol/engine"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	admissionv1 "k8s.io/api/admission/v1"
	registrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

type fixture struct {
	Label         string                       `json:"label"`
	Policy        string                       `json:"policy"`
	Object        *unstructured.Unstructured   `json:"object"`
	OldObject     *unstructured.Unstructured   `json:"oldObject"`
	Actor         string                       `json:"actor"`
	Operation     admissionv1.Operation        `json:"operation"`
	Subresource   string                       `json:"subresource"`
	Expected      string                       `json:"expected"`
	Parents       []*unstructured.Unstructured `json:"parents"`
	LookupFailure string                       `json:"lookupFailure"`
}

type fixtureContext struct {
	libs.Context
	forbidden bool
}

func (c fixtureContext) GetResource(apiVersion, resource, namespace, name string) (*unstructured.Unstructured, error) {
	if c.forbidden {
		return nil, apierrors.NewForbidden(schema.GroupResource{Group: "kro.run", Resource: "webapps"}, name, errors.New("fictional read refusal"))
	}
	return c.Context.GetResource(apiVersion, resource, namespace, name)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	policies := map[string]*policiesv1beta1.ValidatingPolicy{}
	for _, path := range os.Args[1:] {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The pinned upstream CLI also decodes v1 into its v1beta1 engine
		// representation (the Spec is an alias); preserve the authored GVK.
		var policy policiesv1beta1.ValidatingPolicy
		if err := yaml.UnmarshalStrict(data, &policy); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		policies[policy.Name] = &policy
	}
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	count := 0
	for {
		var f fixture
		if err := decoder.Decode(&f); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if err := evaluate(f, policies); err != nil {
			return fmt.Errorf("%s: %w", f.Label, err)
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no admission controls executed")
	}
	fmt.Printf("Tenant WebApp boundary: %d explicit Kyverno engine controls passed.\n", count)
	return nil
}

func evaluate(f fixture, policies map[string]*policiesv1beta1.ValidatingPolicy) error {
	policy, ok := policies[f.Policy]
	if !ok || f.Label == "" || f.Actor == "" || (f.Expected != "pass" && f.Expected != "fail" && f.Expected != "error") {
		return fmt.Errorf("incomplete fixture")
	}
	if (f.LookupFailure != "" && f.LookupFailure != "forbidden") || (f.Expected == "error" && policy.GetFailurePolicy(false) != registrationv1.Fail) {
		return fmt.Errorf("invalid lookup control or policy does not fail closed")
	}
	if (f.Operation != admissionv1.Create && f.Operation != admissionv1.Update && f.Operation != admissionv1.Delete) ||
		(f.Operation == admissionv1.Update && (f.OldObject == nil || f.Object == nil)) ||
		(f.Operation == admissionv1.Create && (f.Object == nil || f.OldObject != nil)) ||
		(f.Operation == admissionv1.Delete && (f.Object != nil || f.OldObject == nil)) {
		return fmt.Errorf("invalid admission operation/old object")
	}
	target := f.Object
	if target == nil {
		target = f.OldObject
	}
	resources := map[string]string{"WebApp": "webapps", "Deployment": "deployments", "Service": "services", "HTTPRoute": "httproutes"}
	gvk := target.GroupVersionKind()
	resource, ok := resources[gvk.Kind]
	if !ok || gvk.Version == "" {
		return fmt.Errorf("unsupported fixture kind")
	}
	// A typed nil pointer in runtime.Object is non-nil and panics inside the
	// upstream converter. CREATE has no old object, not a typed-nil object.
	var object, oldObject runtime.Object
	if f.Object != nil {
		object = f.Object
	}
	if f.OldObject != nil {
		oldObject = f.OldObject
	}
	// Only the external resource data is fictional. The expression compiler,
	// request matching and policy evaluation remain the upstream engine.
	contextProvider := libs.NewFakeContextProvider()
	for _, parent := range f.Parents {
		if parent == nil || parent.GetAPIVersion() != "kro.run/v1alpha1" || parent.GetKind() != "WebApp" {
			return fmt.Errorf("invalid fictional context resource")
		}
		if err := contextProvider.AddResource(schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}, parent); err != nil {
			return err
		}
	}
	// Resource-library functions capture this context at compilation time in
	// Kyverno 1.19.1. Compile after loading each independent fixture, as the CLI
	// does; passing only request.Context would silently use an empty lookup.
	libraryContext := fixtureContext{Context: contextProvider, forbidden: f.LookupFailure == "forbidden"}
	libs.LibraryContext = libraryContext
	provider, err := vpolengine.NewProvider(compiler.NewCompiler(), []policiesv1beta1.ValidatingPolicyLike{policy}, nil)
	if err != nil {
		return err
	}
	eng := vpolengine.NewEngine(provider, func(string) *corev1.Namespace { return nil }, matching.NewMatcher())
	req := celengine.Request(libraryContext, gvk, schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: resource},
		f.Subresource, target.GetName(), target.GetNamespace(), f.Operation,
		authenticationv1.UserInfo{Username: f.Actor}, object, oldObject, false, nil)
	response, err := eng.Handle(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if len(response.Policies) != 1 || len(response.Policies[0].Rules) == 0 {
		return fmt.Errorf("policy did not evaluate: %+v", response.Policies)
	}
	failed, evaluationError := false, false
	for _, rule := range response.Policies[0].Rules {
		switch rule.Status() {
		case engineapi.RuleStatusPass:
		case engineapi.RuleStatusFail:
			failed = true
		case engineapi.RuleStatusError:
			evaluationError = true
		default:
			return fmt.Errorf("unexpected rule status %s: %s", rule.Status(), rule.Message())
		}
	}
	if (f.Expected == "error" && !evaluationError) || (f.Expected != "error" && evaluationError) || (f.Expected != "error" && failed != (f.Expected == "fail")) {
		return fmt.Errorf("expected %s, failed=%v error=%v: %+v", f.Expected, failed, evaluationError, response.Policies[0].Rules)
	}
	return nil
}
