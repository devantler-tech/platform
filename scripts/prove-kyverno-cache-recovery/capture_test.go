package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func capturedResponses(t *testing.T) map[string][]byte {
	t.Helper()
	d, p, r, pods := controllerFixture()
	for i, pod := range pods {
		pod["status"].(object)["podIP"] = fmt.Sprintf("10.0.0.%d", i+1)
	}
	s, e, _ := serviceFixture(t)
	return map[string][]byte{
		"/apis/policies.kyverno.io/v1/imagevalidatingpolicies/verify-app-images": policyFixture(t, nil),
		"/api/v1/namespaces/default": encoded(t, object{"apiVersion": "v1", "kind": "Namespace", "metadata": object{"name": "default", "uid": "namespace-1", "resourceVersion": "3", "labels": object{"kubernetes.io/metadata.name": "default"}}, "status": object{"phase": "Active"}}),
		"/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/kyverno-resource-validating-webhook-cfg":                   webhookFixture(t, nil),
		"/apis/policies.kyverno.io/v1/policyexceptions?limit=1000":                                                                        nativeList(t, "policies.kyverno.io/v1", "PolicyExceptionList", []object{}),
		"/apis/apps/v1/namespaces/kyverno/deployments/kyverno-admission-controller":                                                       encoded(t, d),
		"/apis/policy/v1/namespaces/kyverno/poddisruptionbudgets/kyverno-admission-controller":                                            encoded(t, p),
		"/apis/apps/v1/namespaces/kyverno/replicasets?limit=1000":                                                                         nativeList(t, "apps/v1", "ReplicaSetList", []object{r}),
		"/api/v1/namespaces/kyverno/pods?limit=1000":                                                                                      nativeList(t, "v1", "PodList", pods),
		"/api/v1/namespaces/kyverno/services/kyverno-svc":                                                                                 encoded(t, s),
		"/apis/discovery.k8s.io/v1/namespaces/kyverno/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3Dkyverno-svc&limit=1000": nativeList(t, "discovery.k8s.io/v1", "EndpointSliceList", []object{e}),
		"/api/v1/namespaces/kyverno/secrets/ghcr-auth":                                                                                    credentialSecret(t, `{"auths":{"ghcr.io":{"username":"probe","password":"private-test-token"}}}`, nil),
	}
}

func readRunner(t *testing.T, responses map[string][]byte, calls *int) commandFunc {
	t.Helper()
	return func(_ context.Context, program string, args []string, input []byte, env map[string]string) ([]byte, error) {
		if program != "kubectl" || len(args) != 6 || !reflect.DeepEqual(args[:5], []string{"--context", "admin@prod", "--request-timeout=15s", "get", "--raw"}) || len(input) != 0 || len(env) != 0 {
			t.Fatal("snapshot issued an unexpected command or forwarded registry credentials")
		}
		*calls++
		raw, ok := responses[args[5]]
		if !ok {
			return nil, errors.New("private external failure")
		}
		return raw, nil
	}
}

func TestSnapshotQualifiesRealNativeEnvelopesBeforeReturningCredential(t *testing.T) {
	calls := 0
	s, err := takeSnapshot(context.Background(), readRunner(t, capturedResponses(t), &calls), []byte(desiredPolicySpec))
	if err != nil || len(s.objects) != 11 || s.username != "probe" || s.password != "private-test-token" || calls != 11 {
		t.Fatalf("qualified snapshot failed: objects=%d calls=%d err=%v", len(s.objects), calls, err)
	}
}

func TestSnapshotFailureReturnsNoPartialBindingsOrPrivateError(t *testing.T) {
	for _, path := range []string{"/api/v1/namespaces/kyverno/secrets/ghcr-auth", "/api/v1/namespaces/default"} {
		responses := capturedResponses(t)
		delete(responses, path)
		calls := 0
		s, err := takeSnapshot(context.Background(), readRunner(t, responses, &calls), []byte(desiredPolicySpec))
		if err == nil || len(s.objects) != 0 || s.username != "" || s.password != "" || strings.Contains(err.Error(), "private external failure") {
			t.Fatal("failed snapshot exposed a partial binding or private error")
		}
	}
}
