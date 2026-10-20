// This offline test driver uses the deployed Kyverno compiler and admission
// engine. Unlike kyverno apply/test 1.19.1, it preserves distinct old/new
// objects and an explicit subresource. It never connects to Kubernetes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	celengine "github.com/kyverno/kyverno/pkg/cel/engine"
	"github.com/kyverno/kyverno/pkg/cel/matching"
	"github.com/kyverno/kyverno/pkg/cel/policies/vpol/compiler"
	vpolengine "github.com/kyverno/kyverno/pkg/cel/policies/vpol/engine"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

type fixture struct {
	Label       string                     `json:"label"`
	Policy      string                     `json:"policy"`
	Object      *unstructured.Unstructured `json:"object"`
	OldObject   *unstructured.Unstructured `json:"oldObject"`
	Actor       string                     `json:"actor"`
	Operation   admissionv1.Operation      `json:"operation"`
	Subresource string                     `json:"subresource"`
	Expected    string                     `json:"expected"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	engines := map[string]vpolengine.Engine{}
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
		provider, err := vpolengine.NewProvider(compiler.NewCompiler(), []policiesv1beta1.ValidatingPolicyLike{&policy}, nil)
		if err != nil {
			return err
		}
		engines[policy.Name] = vpolengine.NewEngine(provider, func(string) *corev1.Namespace { return nil }, matching.NewMatcher())
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
		if err := evaluate(f, engines); err != nil {
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

func evaluate(f fixture, engines map[string]vpolengine.Engine) error {
	eng, ok := engines[f.Policy]
	if !ok || f.Label == "" || f.Actor == "" || f.Object == nil || (f.Expected != "pass" && f.Expected != "fail") {
		return fmt.Errorf("incomplete fixture")
	}
	if (f.Operation != admissionv1.Create && f.Operation != admissionv1.Update) || (f.Operation == admissionv1.Update && f.OldObject == nil) {
		return fmt.Errorf("invalid admission operation/old object")
	}
	resources := map[string]string{"WebApp": "webapps", "Deployment": "deployments", "Service": "services", "HTTPRoute": "httproutes"}
	gvk := f.Object.GroupVersionKind()
	resource, ok := resources[gvk.Kind]
	if !ok || gvk.Version == "" {
		return fmt.Errorf("unsupported fixture kind")
	}
	// A typed nil pointer in runtime.Object is non-nil and panics inside the
	// upstream converter. CREATE has no old object, not a typed-nil object.
	var oldObject runtime.Object
	if f.OldObject != nil {
		oldObject = f.OldObject
	}
	req := celengine.Request(nil, gvk, schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: resource},
		f.Subresource, f.Object.GetName(), f.Object.GetNamespace(), f.Operation,
		authenticationv1.UserInfo{Username: f.Actor}, f.Object, oldObject, false, nil)
	response, err := eng.Handle(context.Background(), req, nil)
	if err != nil {
		return err
	}
	if len(response.Policies) != 1 || len(response.Policies[0].Rules) == 0 {
		return fmt.Errorf("policy did not evaluate: %+v", response.Policies)
	}
	failed := false
	for _, rule := range response.Policies[0].Rules {
		switch rule.Status() {
		case engineapi.RuleStatusPass:
		case engineapi.RuleStatusFail:
			failed = true
		default:
			return fmt.Errorf("unexpected rule status %s: %s", rule.Status(), rule.Message())
		}
	}
	if failed != (f.Expected == "fail") {
		return fmt.Errorf("expected %s, failed=%v: %+v", f.Expected, failed, response.Policies[0].Rules)
	}
	return nil
}
