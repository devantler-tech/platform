package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestRealAdmissionResponseMustMatchControl(t *testing.T) {
	valid := fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"longhorn-cache-signed-123","namespace":"default"},"spec":{"containers":[{"name":"probe","image":%q}]}}`, signedImage)
	if !admitted([]byte(valid), "longhorn-cache-signed-123", signedImage) {
		t.Fatal("matching admitted Pod was not recognized")
	}
	for _, body := range []string{`{}`, valid + `{}`, fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"foreign","namespace":"default"},"spec":{"containers":[{"name":"probe","image":%q}]}}`, signedImage)} {
		if admitted([]byte(body), "longhorn-cache-signed-123", signedImage) {
			t.Fatal("unrelated or incomplete response accepted as signed control")
		}
	}
	if admitted([]byte(valid), "longhorn-cache-signed-123", unsignedImage) {
		t.Fatal("rewritten image accepted")
	}
}

func TestOnlyIntendedPolicyRejectionCounts(t *testing.T) {
	denial := []byte(`admission webhook "ivpol.validate.kyverno.svc-fail-finegrained-verify-app-images" denied the request: Policy verify-app-images failed: first-party app image is not signed by the publish-app release pipeline`)
	if !policyDenied(denial, errors.New("API refused")) {
		t.Fatal("intended policy denial was not recognized")
	}
	for _, body := range []string{
		`Error from server (Forbidden): pods is forbidden`,
		`Error from server (Invalid): Pod violates PodSecurity`,
		`failed calling webhook: connection timeout`,
		`admission webhook "other-policy" denied the request: Policy verify-app-images failed`,
		`admission webhook "ivpol.validate.kyverno.svc-fail-finegrained-verify-app-images" failed calling webhook: no endpoints available`,
		`admission webhook "ivpol.validate.kyverno.svc-fail-finegrained-verify-app-images" denied the request: Policy verify-app-images failed: failed to get imagedata`,
		`admission webhook "ivpol.validate.kyverno.svc-fail-finegrained-verify-app-images" denied the request: Policy verify-app-images failed: another validation failed`,
	} {
		if policyDenied([]byte(body), errors.New("failed")) {
			t.Fatalf("unrelated refusal counted: %s", body)
		}
	}
	if policyDenied(denial, nil) {
		t.Fatal("successful request counted as denial")
	}
}
