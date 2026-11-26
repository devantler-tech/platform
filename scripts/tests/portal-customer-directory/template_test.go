package portaldirectory

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

func manifest(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func field(t *testing.T, doc map[string]any, keys ...string) any {
	t.Helper()
	var value any = doc
	for _, key := range keys {
		mapping, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s is not a mapping", key)
		}
		value = mapping[key]
	}
	return value
}

func render(t *testing.T, source string) string {
	t.Helper()
	funcs := template.FuncMap{
		"b64enc": func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) },
	}
	tpl, err := template.New("runtime").Funcs(funcs).Option("missingkey=error").Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := tpl.Execute(&output, map[string]string{
		"portal_web_password":         "synthetic-password",
		"portal_zitadel_org_id":       "123456789",
		"portal_zitadel_token":        "synthetic-token",
		"portal_preview_web_password": "fictional+password",
		"portal_preview_database_ca":  previewFixtureCA,
	}); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

const previewFixtureCA = "Fictional preview certificate\nSecond line\n"

func TestRuntimeSettingsEnableTheCustomerDirectory(t *testing.T) {
	doc := manifest(t, "k8s/providers/hetzner/apps/business-site/external-secret-runtime.yaml")
	source, ok := field(t, doc, "spec", "target", "template", "data", "portal-settings").(string)
	if !ok {
		t.Fatal("missing runtime settings template")
	}
	const activation = "$portalCustomerDirectoryEnabled := true"
	if strings.Count(source, activation) != 1 {
		t.Fatal("the customer directory is disabled in production")
	}
	enabled := render(t, source)
	for _, expected := range []string{
		"PORTAL_ZITADEL_URL=https://accounts.devantler.tech\n",
		"PORTAL_ZITADEL_ORG_ID=123456789\n",
		"PORTAL_ZITADEL_TOKEN_PROJECTION_FILE=/run/webapp-inputs/portal-zitadel-token\n",
		"PORTAL_CLIENT_PREVIEW_DATABASE_URL=postgres://portal_preview_web:fictional%2Bpassword@portal-preview-db-rw.business-site-dev.svc.cluster.local:5432/portal_preview?sslmode=verify-full\n",
		"PORTAL_CLIENT_PREVIEW_DATABASE_CA=" + base64.StdEncoding.EncodeToString([]byte(previewFixtureCA)) + "\n",
	} {
		if !strings.Contains(enabled, expected) {
			t.Errorf("activated settings omitted %q", expected)
		}
	}
	if !strings.Contains(enabled, "PORTAL_OIDC_CLIENT_ID=business-portal\n") {
		t.Fatal("directory activation removed existing sign-in configuration")
	}
	if strings.Contains(render(t, strings.Replace(source, activation, "$portalCustomerDirectoryEnabled := false", 1)), "PORTAL_ZITADEL_") {
		t.Fatal("disabling the directory must remove all three settings")
	}
}

func TestRuntimeTokenAndOrganizationUseTheApprovedExactRecord(t *testing.T) {
	doc := manifest(t, "k8s/providers/hetzner/apps/business-site/external-secret-runtime.yaml")
	if got := field(t, doc, "spec", "target", "template", "data", "portal-zitadel-token"); got != "{{ .portal_zitadel_token }}" {
		t.Fatalf("wrong projected token template: %v", got)
	}
	want := map[string]string{"portal_zitadel_token": "token", "portal_zitadel_org_id": "org-id"}
	items, ok := field(t, doc, "spec", "data").([]any)
	if !ok {
		t.Fatal("missing ExternalSecret data")
	}
	for _, item := range items {
		entry := item.(map[string]any)
		key, _ := entry["secretKey"].(string)
		property, selected := want[key]
		if !selected {
			continue
		}
		if field(t, entry, "remoteRef", "key") != "apps/business-site/portal-zitadel" ||
			field(t, entry, "remoteRef", "property") != property ||
			field(t, entry, "sourceRef", "storeRef", "name") != "openbao" ||
			field(t, entry, "sourceRef", "storeRef", "kind") != "ClusterSecretStore" {
			t.Errorf("%s does not read the approved record and shared store", key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing remote inputs: %v", want)
	}
}

func TestIdentityPushPublishesTheMatchingOrganization(t *testing.T) {
	doc := manifest(t, "k8s/providers/hetzner/apps/customer-identity/push-secret-portal-zitadel-token.yaml")
	items := field(t, doc, "spec", "data").([]any)
	if len(items) != 2 {
		t.Fatal("the exact record must contain its token and proved organization")
	}
	want := map[string]bool{"token": true, "org-id": true}
	for _, item := range items {
		match := field(t, item.(map[string]any), "match").(map[string]any)
		key, _ := match["secretKey"].(string)
		if !want[key] || field(t, match, "remoteRef", "property") != key ||
			field(t, match, "remoteRef", "remoteKey") != "apps/business-site/portal-zitadel" {
			t.Fatalf("unexpected push mapping: %v", key)
		}
		delete(want, key)
	}
}
