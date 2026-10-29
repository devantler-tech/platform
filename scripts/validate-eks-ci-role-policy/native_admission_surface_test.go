package main

import "testing"

func TestNativeAdmissionPoliciesAndBindingsCannotEscapeTheApprovedSurface(t *testing.T) {
	for _, kind := range []string{"MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		t.Run(kind, func(t *testing.T) {
			identity := resourceIdentity{apiVersion: "admissionregistration.k8s.io/v1", kind: kind, name: "example"}
			document := map[string]any{"apiVersion": identity.apiVersion, "kind": kind, "metadata": map[string]any{"name": "example"}, "spec": map[string]any{}}
			if !isAuthorizationResource(document, identity) {
				t.Fatal("native admission control escaped authorization-surface selection")
			}
			if !isAuthorizationKind(kind) || !isAuthorizationKind("admissionregistration.k8s.io/v1/"+kind) {
				t.Fatal("embedded native admission control escaped selection")
			}
		})
	}
}

func TestEmbeddedNativeAdmissionGroupWildcardsAreProtected(t *testing.T) {
	for _, kind := range []string{"admissionregistration.k8s.io/*", "admissionregistration.k8s.io/v1/*"} {
		if !isAuthorizationKind(kind) {
			t.Fatal("native admission wildcard escaped protected authorization surface")
		}
	}
}
