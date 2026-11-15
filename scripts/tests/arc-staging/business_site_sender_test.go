package arcstaging_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// businessSiteSeedGrants refuses any seed grant that can reach the business
// site's prefix other than the two exact SMTP credential grants, including a
// glob (`*`) or segment wildcard (`+`) that covers it.
func businessSiteSeedGrants(policy string) error {
	stanzas := regexp.MustCompile(`(?s)path\s+"([^"]*)"\s*\{\s*capabilities\s*=\s*(\[[^\]]+\])\s*\}`).FindAllStringSubmatch(policy, -1)
	if len(stanzas) == 0 || len(stanzas) != strings.Count(policy, "path \"") {
		return errors.New("every seed policy path must parse as one capabilities stanza")
	}
	probes := []string{
		"secret/data/apps/business-site/smtp",
		"secret/data/apps/business-site/other",
		"secret/metadata/apps/business-site/smtp",
		"secret/metadata/apps/business-site/other",
	}
	want := map[string]bool{
		"secret/data/apps/business-site/smtp":     true,
		"secret/metadata/apps/business-site/smtp": true,
	}
	for _, stanza := range stanzas {
		pattern := regexp.QuoteMeta(stanza[1])
		pattern = strings.ReplaceAll(pattern, `\+`, `[^/]+`)
		if strings.HasSuffix(pattern, `\*`) {
			pattern = strings.TrimSuffix(pattern, `\*`) + `.*`
		}
		matcher := regexp.MustCompile(`^` + pattern + `$`)
		if !slices.ContainsFunc(probes, matcher.MatchString) {
			continue
		}
		if !want[stanza[1]] {
			return errors.New("the business site's seed authorization is duplicated or broader than its SMTP credential: " + stanza[1])
		}
		delete(want, stanza[1])
		var capabilities []string
		if err := json.Unmarshal([]byte(stanza[2]), &capabilities); err != nil {
			return err
		}
		slices.Sort(capabilities)
		if !reflect.DeepEqual(capabilities, []string{"create", "read", "update"}) {
			return errors.New("the SMTP credential grant must write and read without list or delete authority")
		}
	}
	if len(want) != 0 {
		return errors.New("need exactly one data and one metadata grant for the business site's SMTP credential")
	}
	return nil
}

// The business site's send-only SES credential (platform#5400) reaches
// OpenBao only through its platform PushSecret, at one exact key under the
// business site's own prefix, and may send only from its one sender address.
func TestBusinessSiteSenderCredentialIsExactAndSendOnly(t *testing.T) {
	_, seed := captureBaoWrite(t, "bao", "policy", "write", "vault-seed-write")
	if err := businessSiteSeedGrants(seed); err != nil {
		t.Fatal(err)
	}
	const exact = `path "secret/data/apps/business-site/smtp" {
                capabilities = ["create", "update", "read"]
              }`
	for name, widened := range map[string]string{
		"apps glob":        `path "secret/data/apps/*" { capabilities = ["read"] }`,
		"secret glob":      `path "secret/*" { capabilities = ["read"] }`,
		"prefix glob":      `path "secret/data/apps/business-site*" { capabilities = ["read"] }`,
		"segment wildcard": `path "secret/metadata/apps/+/smtp" { capabilities = ["read"] }`,
		"other key":        `path "secret/data/apps/business-site/other" { capabilities = ["read"] }`,
		"duplicate":        exact,
	} {
		if businessSiteSeedGrants(seed+"\n"+widened+"\n") == nil {
			t.Fatalf("a %s grant over the business site's prefix was accepted", name)
		}
	}
	withDelete := regexp.MustCompile(`(path\s+"secret/data/apps/business-site/smtp"\s*\{\s*capabilities\s*=\s*\[)`).ReplaceAllString(seed, `${1}"delete", `)
	if withDelete == seed {
		t.Fatal("the delete control did not change the seed policy")
	}
	if businessSiteSeedGrants(withDelete) == nil {
		t.Fatal("a delete capability on the business site's SMTP credential was accepted")
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
