package arcstaging_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"testing"
)

// The business site's send-only SES credential (platform#5400) reaches
// OpenBao only through its platform PushSecret, at one exact key under the
// business site's own prefix, and may send only from its one sender address.
func TestBusinessSiteSenderCredentialIsExactAndSendOnly(t *testing.T) {
	_, seed := captureBaoWrite(t, "bao", "policy", "write", "vault-seed-write")
	entries := regexp.MustCompile(`(?s)path\s+"(secret/(?:data|metadata)/apps/business-site/[^"]*)"\s*\{\s*capabilities\s*=\s*(\[[^\]]+\])\s*\}`).FindAllStringSubmatch(seed, -1)
	want := map[string]bool{
		"secret/data/apps/business-site/smtp":     true,
		"secret/metadata/apps/business-site/smtp": true,
	}
	if len(entries) != len(want) {
		t.Fatalf("need exactly one data and one metadata grant for the business site's SMTP credential, got %d", len(entries))
	}
	for _, entry := range entries {
		if !want[entry[1]] {
			t.Fatal("the business site's seed authorization is duplicated or broader than its SMTP credential")
		}
		delete(want, entry[1])
		var capabilities []string
		if err := json.Unmarshal([]byte(entry[2]), &capabilities); err != nil {
			t.Fatal(err)
		}
		slices.Sort(capabilities)
		if !reflect.DeepEqual(capabilities, []string{"create", "read", "update"}) {
			t.Fatal("the SMTP credential grant must write and read without list or delete authority")
		}
	}

	push := readYAML(t, "k8s/providers/hetzner/apps/aws/push-secret-business-site-smtp.yaml")
	equal(t, mailField[string](t, push, "metadata", "namespace"), "aws")
	equal(t, mailField[string](t, push, "spec", "selector", "secret", "name"), "business-site-smtp-credentials")
	equal(t, mailField[string](t, push, "spec", "updatePolicy"), "Replace")
	equal(t, mailField[string](t, push, "spec", "deletionPolicy"), "None")
	data := mailField[[]any](t, push, "spec", "data")
	equal(t, len(data), 2)
	pushed := map[string]string{}
	for _, entry := range data {
		equal(t, mailField[string](t, entry, "match", "remoteRef", "remoteKey"), "apps/business-site/smtp")
		pushed[mailField[string](t, entry, "match", "secretKey")] = mailField[string](t, entry, "match", "remoteRef", "property")
	}
	equal(t, pushed["username"], "username")
	equal(t, pushed["attribute.ses_smtp_password_v4"], "password")

	key := readYAML(t, "k8s/providers/hetzner/apps/aws/access-key-business-site-smtp.yaml")
	equal(t, mailField[string](t, key, "spec", "forProvider", "userRef", "name"), "business-site-smtp")
	equal(t, mailField[string](t, key, "spec", "writeConnectionSecretToRef", "name"), "business-site-smtp-credentials")
	attachment := readYAML(t, "k8s/providers/hetzner/apps/aws/user-policy-attachment-business-site-smtp.yaml")
	equal(t, mailField[string](t, attachment, "spec", "forProvider", "userRef", "name"), "business-site-smtp")
	equal(t, mailField[string](t, attachment, "spec", "forProvider", "policyArnRef", "name"), "business-site-smtp-send")
	for _, path := range []string{
		"user-business-site-smtp.yaml",
		"policy-business-site-smtp-send.yaml",
		"user-policy-attachment-business-site-smtp.yaml",
		"access-key-business-site-smtp.yaml",
	} {
		policies := mailField[[]any](t, readYAML(t, "k8s/providers/hetzner/apps/aws/"+path), "spec", "managementPolicies")
		if slices.Contains(policies, any("Delete")) || slices.Contains(policies, any("*")) {
			t.Fatalf("%s must not let a prune delete the sender identity", path)
		}
	}

	var document struct {
		Version   string
		Statement []map[string]any
	}
	source := mailField[string](t, readYAML(t, "k8s/providers/hetzner/apps/aws/policy-business-site-smtp-send.yaml"), "spec", "forProvider", "policy")
	if err := json.Unmarshal([]byte(source), &document); err != nil {
		t.Fatal(err)
	}
	wantStatement := map[string]any{
		"Sid":      "SendBusinessSiteMail",
		"Effect":   "Allow",
		"Action":   "ses:SendRawEmail",
		"Resource": "arn:aws:ses:eu-central-1:939001610192:identity/devantler.tech",
		"Condition": map[string]any{
			"StringEquals": map[string]any{"ses:FromAddress": "no-reply@devantler.tech"},
		},
	}
	if document.Version != "2012-10-17" || len(document.Statement) != 1 || !reflect.DeepEqual(document.Statement[0], wantStatement) {
		t.Fatal("the business site's SMTP identity may only send raw mail from its one sender address through the company mail domain")
	}
}
