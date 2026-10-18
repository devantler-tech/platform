package admission_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	registrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The real API server evaluates the chart-rendered conditions before calling a
// deliberately rejecting TLS webhook. No Kyverno mock, user kubeconfig or
// production identity is involved.
func TestRenderedConditionsBreakReviewWebhookRecursion(t *testing.T) {
	data, err := os.ReadFile(os.Getenv("KYVERNO_MATCH_CONDITIONS"))
	if err != nil {
		t.Fatal(err)
	}
	var conditions []registrationv1.MatchCondition
	if err := json.Unmarshal(data, &conditions); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("verified local API assets required")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory:    os.Getenv("KUBEBUILDER_ASSETS"),
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: time.Minute,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review admissionv1.AdmissionReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Request == nil {
			http.Error(w, "invalid fixture admission request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		review.Response = &admissionv1.AdmissionResponse{UID: review.Request.UID, Allowed: false,
			Result: &metav1.Status{Message: "fixture webhook reached"}}
		review.Request = nil
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	fail, none := registrationv1.Fail, registrationv1.SideEffectClassNone
	timeout := int32(2)
	url := server.URL
	webhook := registrationv1.ValidatingWebhook{
		Name: "auth-review.fixture.test", AdmissionReviewVersions: []string{"v1"},
		ClientConfig: registrationv1.WebhookClientConfig{URL: &url,
			CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})},
		FailurePolicy: &fail, SideEffects: &none, TimeoutSeconds: &timeout,
		Rules: []registrationv1.RuleWithOperations{{Operations: []registrationv1.OperationType{registrationv1.Create, registrationv1.Update},
			Rule: registrationv1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*"}}}},
	}
	probe := func() error {
		_, err := client.CoreV1().ConfigMaps("default").Create(ctx,
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "registration-probe"}}, metav1.CreateOptions{})
		return err
	}
	install := func(name string, match []registrationv1.MatchCondition) {
		t.Helper()
		webhook.MatchConditions = match
		_, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx,
			&registrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: []registrationv1.ValidatingWebhook{webhook}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Admission registration propagates asynchronously in the real API server.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if err := probe(); err != nil && strings.Contains(err.Error(), "fixture webhook reached") {
				return
			}
			_ = client.CoreV1().ConfigMaps("default").Delete(ctx, "registration-probe", metav1.DeleteOptions{})
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("webhook never became active")
	}
	sar := func() error {
		_, err := client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{User: "fixture-user", ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "default", Verb: "get", Resource: "pods"}},
		}, metav1.CreateOptions{})
		return err
	}
	// Positive control: today's missing server-side exclusion reaches the webhook.
	install("before", nil)
	if err := sar(); err == nil || !strings.Contains(err.Error(), "fixture webhook reached") {
		t.Fatalf("unfiltered authorization review must reach the rejecting webhook, got %v", err)
	}
	if err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, "before", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Avoid a propagation race with the positive control before installing after.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := sar(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unfiltered fixture webhook did not retire")
		}
		time.Sleep(50 * time.Millisecond)
	}
	install("after", conditions)
	for name, request := range map[string]func() error{
		"SubjectAccessReview": sar,
		"SelfSubjectAccessReview": func() error {
			_, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
				&authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: "get", Resource: "pods"}}}, metav1.CreateOptions{})
			return err
		},
		"TokenReview": func() error {
			_, err := client.AuthenticationV1().TokenReviews().Create(ctx,
				&authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: "synthetic-invalid-token"}}, metav1.CreateOptions{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := calls.Load()
			if err := request(); err != nil {
				t.Fatalf("already-filtered review called the webhook: %v", err)
			}
			if calls.Load() != before {
				t.Fatal("already-filtered review still dispatched")
			}
		})
	}
	for name, request := range map[string]func() error{
		"Pod": func() error {
			_, err := client.CoreV1().Pods("default").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fixture", Image: "fixture.invalid/image:v1"}}}}, metav1.CreateOptions{})
			return err
		},
		"Secret": func() error {
			_, err := client.CoreV1().Secrets("default").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}}, metav1.CreateOptions{})
			return err
		},
		"Role": func() error {
			_, err := client.RbacV1().Roles("default").Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}}, metav1.CreateOptions{})
			return err
		},
		"ClusterRoleBinding": func() error {
			_, err := client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "view"}}, metav1.CreateOptions{})
			return err
		},
		"LocalSubjectAccessReview": func() error {
			_, err := client.AuthorizationV1().LocalSubjectAccessReviews("default").Create(ctx, &authorizationv1.LocalSubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: "fixture-user", ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "default", Verb: "get", Resource: "pods"}}}, metav1.CreateOptions{})
			return err
		},
		"SelfSubjectRulesReview": func() error {
			_, err := client.AuthorizationV1().SelfSubjectRulesReviews().Create(ctx, &authorizationv1.SelfSubjectRulesReview{Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: "default"}}, metav1.CreateOptions{})
			return err
		},
	} {
		t.Run(name+" remains fail closed", func(t *testing.T) {
			before := calls.Load()
			if err := request(); err == nil || !strings.Contains(err.Error(), "fixture webhook reached") {
				t.Fatalf("request bypassed rejecting webhook: %v", err)
			}
			// The API server may retry a URL webhook delivery. The security
			// contract is that an ordinary request reaches the rejecting
			// webhook at least once and remains rejected, not that the
			// transport performs exactly one HTTP exchange.
			if calls.Load() <= before {
				t.Fatal("expected a rejecting webhook call")
			}
		})
	}
	// CEL errors also remain fail closed, rather than turning into an exclusion.
	after, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "after", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after.Webhooks[0].MatchConditions = []registrationv1.MatchCondition{{Name: "evaluation-error", Expression: "object.metadata.labels['missing-label'] == 'value'"}}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, after, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		err := probe()
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "resulted in error") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CEL evaluation errors did not fail closed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
