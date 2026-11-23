package compiler_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kubernetes-sigs/kro/pkg/features"
	crdcompat "github.com/kubernetes-sigs/kro/pkg/graph/crd/compat"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// uninstalledAPIChange refuses a change to a generated API that KRO would
// never install. KRO updates an existing CRD only when its compatibility report
// lists a change or the names changed (CRDWrapper.Ensure in pkg/client/crd.go).
// Without the ConservativeCRDComparison feature gate, that report ignores
// x-kubernetes-validations, format and other unclassified schema fields. Such a
// change passes every fixture that installs the generated CRD directly, while
// the cluster keeps the old API (platform#6420).
func uninstalledAPIChange(installed, desired *extv1.CustomResourceDefinition) error {
	if equality.Semantic.DeepEqual(installed.Spec, desired.Spec) {
		return nil
	}
	report, err := crdcompat.CompareVersions(installed.Spec.Versions, desired.Spec.Versions)
	if err != nil {
		return fmt.Errorf("compare the generated API with KRO: %w", err)
	}
	if report.HasChanges() || !equality.Semantic.DeepEqual(installed.Spec.Names, desired.Spec.Names) {
		return nil
	}
	return fmt.Errorf("the generated %s API changes, but KRO's compatibility check sees no change, "+
		"so KRO keeps the installed API and the change never reaches the cluster; "+
		"also change a field KRO compares, such as a description", desired.Spec.Names.Kind)
}

// tenantAPI is a minimal generated API: one served version whose spec.source
// carries the given CEL rules.
func tenantAPI(rules ...string) *extv1.CustomResourceDefinition {
	validations := make([]extv1.ValidationRule, 0, len(rules))
	for _, rule := range rules {
		validations = append(validations, extv1.ValidationRule{Rule: rule, Message: "validation failed"})
	}
	return &extv1.CustomResourceDefinition{
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: "kro.run",
			Names: extv1.CustomResourceDefinitionNames{Kind: "Tenant", ListKind: "TenantList", Plural: "tenants", Singular: "tenant"},
			Scope: extv1.NamespaceScoped,
			Versions: []extv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &extv1.CustomResourceValidation{OpenAPIV3Schema: &extv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]extv1.JSONSchemaProps{"spec": {
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{"source": {
							Type:         "object",
							Properties:   map[string]extv1.JSONSchemaProps{"digest": {Type: "string"}},
							XValidations: validations,
						}},
					}},
				}},
			}},
		},
	}
}

func sourceSchema(api *extv1.CustomResourceDefinition) (*extv1.JSONSchemaProps, func(extv1.JSONSchemaProps)) {
	root := api.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	source := spec.Properties["source"]
	return &source, func(changed extv1.JSONSchemaProps) {
		spec.Properties["source"] = changed
		root.Properties["spec"] = spec
	}
}

func TestUninstalledAPIChange(t *testing.T) {
	if features.FeatureGate.Enabled(features.ConservativeCRDComparison) {
		t.Fatal("the check must use the deployed CRD comparison")
	}
	const pinned, released = "self.digest != ''", "self.digest == ''"
	for _, test := range []struct {
		name      string
		change    func(*extv1.CustomResourceDefinition)
		installed bool
	}{
		{name: "unchanged", change: func(*extv1.CustomResourceDefinition) {}, installed: true},
		{name: "rule only", change: func(api *extv1.CustomResourceDefinition) {
			source, set := sourceSchema(api)
			source.XValidations[0].Rule = released
			set(*source)
		}},
		{name: "added rule only", change: func(api *extv1.CustomResourceDefinition) {
			source, set := sourceSchema(api)
			source.XValidations = append(source.XValidations, extv1.ValidationRule{Rule: released})
			set(*source)
		}},
		{name: "format only", change: func(api *extv1.CustomResourceDefinition) {
			source, set := sourceSchema(api)
			digest := source.Properties["digest"]
			digest.Format = "byte"
			source.Properties["digest"] = digest
			set(*source)
		}},
		{name: "rule and description", change: func(api *extv1.CustomResourceDefinition) {
			source, set := sourceSchema(api)
			source.XValidations[0].Rule = released
			source.Description = "Follows signed stable releases."
			set(*source)
		}, installed: true},
		{name: "rule and added property", change: func(api *extv1.CustomResourceDefinition) {
			source, set := sourceSchema(api)
			source.XValidations[0].Rule = released
			source.Properties["version"] = extv1.JSONSchemaProps{Type: "string"}
			set(*source)
		}, installed: true},
		{name: "printer column", change: func(api *extv1.CustomResourceDefinition) {
			api.Spec.Versions[0].AdditionalPrinterColumns = []extv1.CustomResourceColumnDefinition{{Name: "State", Type: "string", JSONPath: ".status.state"}}
		}, installed: true},
		{name: "short name", change: func(api *extv1.CustomResourceDefinition) {
			api.Spec.Names.ShortNames = []string{"tn"}
		}, installed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired := tenantAPI(pinned)
			test.change(desired)
			err := uninstalledAPIChange(tenantAPI(pinned), desired)
			if test.installed && err != nil {
				t.Fatalf("KRO installs this change, but the check refused it: %v", err)
			}
			if !test.installed && (err == nil || !strings.Contains(err.Error(), "never reaches the cluster")) {
				t.Fatalf("KRO keeps the installed API for this change, but the check accepted it: %v", err)
			}
		})
	}
}
