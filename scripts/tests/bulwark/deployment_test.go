// Package bulwark_test pins the security-critical settings of the Bulwark
// webmail (#6662) in the production apps render: how users sign in, what the
// server may reach, the session limits, and that nothing routes to it yet.
package bulwark_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const namespace = "bulwark"

func root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func render(t *testing.T, path string) []map[string]any {
	t.Helper()
	out, err := exec.Command("kubectl", "kustomize", filepath.Join(root(), path)).CombinedOutput()
	if err != nil {
		t.Fatalf("render %s: %v: %s", path, err, out)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	var objects []map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return objects
		} else if err != nil {
			t.Fatal(err)
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
}

func production(t *testing.T) []map[string]any {
	t.Helper()
	return render(t, "k8s/providers/hetzner/apps")
}

func mapping(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected a mapping, got %T", value)
	}
	return result
}

func sequence(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok || len(result) == 0 {
		t.Fatalf("expected a nonempty sequence, got %T", value)
	}
	return result
}

func object(t *testing.T, objects []map[string]any, kind, name string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, item := range objects {
		metadata, _ := item["metadata"].(map[string]any)
		if item["kind"] != kind || metadata["name"] != name {
			continue
		}
		if kind != "Namespace" && metadata["namespace"] != namespace {
			continue
		}
		if found != nil {
			t.Fatalf("duplicate %s %s", kind, name)
		}
		found = item
	}
	if found == nil {
		t.Fatalf("missing %s %s in the production render", kind, name)
	}
	return found
}

func podSpec(t *testing.T, objects []map[string]any) map[string]any {
	t.Helper()
	deployment := object(t, objects, "Deployment", "bulwark")
	return mapping(t, mapping(t, mapping(t, deployment["spec"])["template"])["spec"])
}

func container(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	containers := sequence(t, pod["containers"])
	if len(containers) != 1 || pod["initContainers"] != nil {
		t.Fatal("Bulwark must run as its one container")
	}
	return mapping(t, containers[0])
}

// The sign-in, admin and outside-traffic settings decide who can reach the
// mailbox and what leaves the cluster, so the whole environment is pinned: a
// changed value, a dropped setting or an added one (a custom JMAP endpoint, a
// password fallback, an admin password, a plaintext secret) all fail here.
func TestBulwarkSignsInOnlyThroughStalwartAndSendsNothingElsewhere(t *testing.T) {
	objects := production(t)
	c := container(t, podSpec(t, objects))
	if c["image"] != "ghcr.io/bulwarkmail/webmail:1.22.0@sha256:d02b77b4dea0607dafcd2592a6d89d481b8dd400e4b2b72f5a4466972dc4c5a8" {
		t.Fatalf("Bulwark must run the reviewed, digest-pinned release, got %v", c["image"])
	}
	if c["envFrom"] != nil || c["command"] != nil || c["args"] != nil {
		t.Fatal("Bulwark must run the image's own server with only the pinned environment")
	}
	got := map[string]string{}
	for _, item := range sequence(t, c["env"]) {
		variable := mapping(t, item)
		name, _ := variable["name"].(string)
		value, ok := variable["value"].(string)
		if !ok || variable["valueFrom"] != nil {
			t.Fatalf("%s must be a literal setting; secrets are read from files", name)
		}
		if _, seen := got[name]; seen {
			t.Fatalf("%s is set twice", name)
		}
		got[name] = value
	}
	want := map[string]string{
		"HOSTNAME":                "0.0.0.0",
		"JMAP_SERVER_URL":         "https://stalwart.${domain}",
		"OAUTH_ENABLED":           "true",
		"OAUTH_ONLY":              "true",
		"OAUTH_CLIENT_ID":         "bulwark",
		"NO_THIRD_PARTY_TRAFFIC":  "true",
		"STALWART_FEATURES":       "false",
		"STALWART_ADMIN_ACCESS":   "off",
		"ADMIN_CONFIG_READONLY":   "true",
		"ADMIN_CONFIG_DIR":        "/app/data/admin",
		"ADMIN_STATE_DIR":         "/app/data/admin-state",
		"LOGIN_SHOW_REMEMBER_ME":  "true",
		"BROWSER_SESSION_SIGN_IN": "true",
		"TRUSTED_PROXY_DEPTH":     "1",
		"SESSION_SECRET_FILE":     "/etc/bulwark/session/session-secret",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bulwark's environment must be exactly the reviewed settings\n got: %v\nwant: %v", got, want)
	}
}

// The session secret reaches the container only as a read-only file from the
// generated Secret, which is created once and never rotated under a running
// session. The admin config is a read-only mount; the state directory, which
// Bulwark writes, is on the scratch volume and outside that mount.
func TestBulwarkSecretAndConfigAreReadOnlyFiles(t *testing.T) {
	objects := production(t)
	pod := podSpec(t, objects)
	c := container(t, pod)
	volumes := map[string]map[string]any{}
	for _, item := range sequence(t, pod["volumes"]) {
		volume := mapping(t, item)
		name, _ := volume["name"].(string)
		volumes[name] = volume
		if volume["hostPath"] != nil || volume["persistentVolumeClaim"] != nil {
			t.Fatalf("volume %s: Bulwark keeps nothing on the host or on a claim", name)
		}
	}
	mounts := map[string]map[string]any{}
	for _, item := range sequence(t, c["volumeMounts"]) {
		mount := mapping(t, item)
		path, _ := mount["mountPath"].(string)
		mounts[path] = mount
	}
	session := mounts["/etc/bulwark/session"]
	if session == nil || session["readOnly"] != true {
		t.Fatal("the session secret must be mounted read-only where SESSION_SECRET_FILE points")
	}
	secret := mapping(t, volumes[session["name"].(string)]["secret"])
	if secret["secretName"] != "bulwark" || secret["defaultMode"] != 0o440 || !reflect.DeepEqual(secret["items"], []any{map[string]any{"key": "session-secret", "path": "session-secret"}}) {
		t.Fatalf("the session file must be exactly the generated Secret's session-secret key, readable by the pod's group only, got %v", secret)
	}
	external := mapping(t, object(t, objects, "ExternalSecret", "bulwark")["spec"])
	target := mapping(t, external["target"])
	data := mapping(t, mapping(t, target["template"])["data"])
	generator := mapping(t, mapping(t, mapping(t, sequence(t, external["dataFrom"])[0])["sourceRef"])["generatorRef"])
	if external["refreshInterval"] != "0" || target["name"] != "bulwark" || len(sequence(t, external["dataFrom"])) != 1 ||
		!reflect.DeepEqual(data, map[string]any{"session-secret": "{{ .password }}"}) ||
		generator["kind"] != "Password" || generator["name"] != "bulwark" {
		t.Fatal("the session secret must be generated once by the namespace's Password generator, and nothing else put in that Secret")
	}
	password := mapping(t, object(t, objects, "Password", "bulwark")["spec"])
	if length, _ := password["length"].(int); length < 32 {
		t.Fatal("Bulwark needs a session secret of at least 32 characters")
	}
	config := mounts["/app/data/admin"]
	if config == nil || config["readOnly"] != true || mapping(t, volumes[config["name"].(string)]["configMap"])["name"] != "bulwark-admin-config" {
		t.Fatal("the admin config directory must be the reviewed ConfigMap, mounted read-only")
	}
	for path, volume := range map[string]string{"/app/data": "data", "/app/.next/cache": "next-cache", "/tmp": "tmp"} {
		mount := mounts[path]
		if mount == nil || mount["name"] != volume || volumes[volume]["emptyDir"] == nil {
			t.Fatalf("%s must be a scratch volume", path)
		}
	}
	if len(mounts) != 5 {
		t.Fatalf("unexpected mounts: %v", mounts)
	}
}

// The session limits the maintainer chose: signed out after 15 minutes
// without activity and 12 hours after signing in. Bulwark reads them from
// organisation.session in policy.json (lib/admin/organisation-policy.ts at
// 1.22.0); a misspelled key is silently ignored there, so the keys are pinned.
func TestBulwarkSessionLimits(t *testing.T) {
	data := mapping(t, object(t, production(t), "ConfigMap", "bulwark-admin-config")["data"])
	if len(data) != 2 {
		t.Fatalf("the admin config must hold exactly config.json and policy.json, got %d keys", len(data))
	}
	var policy, config map[string]any
	if err := json.Unmarshal([]byte(data["policy.json"].(string)), &policy); err != nil {
		t.Fatalf("policy.json: %v", err)
	}
	if err := json.Unmarshal([]byte(data["config.json"].(string)), &config); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	want := map[string]any{"organisation": map[string]any{"session": map[string]any{"idleMinutes": 15.0, "maxHours": 12.0, "warnSeconds": 60.0}}}
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("policy.json must set exactly the 15-minute idle and 12-hour maximum session, got %v", policy)
	}
	if !reflect.DeepEqual(config, map[string]any{"setupComplete": true}) {
		t.Fatalf("config.json must only close the setup wizard, got %v", config)
	}
}

// Restricted pod security with no Kubernetes API access.
func TestBulwarkRunsRestricted(t *testing.T) {
	objects := production(t)
	labels := mapping(t, mapping(t, object(t, objects, "Namespace", namespace)["metadata"])["labels"])
	if labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatal("the namespace must enforce restricted pod security")
	}
	if object(t, objects, "ServiceAccount", "bulwark")["automountServiceAccountToken"] != false {
		t.Fatal("Bulwark's service account must not mount a token")
	}
	deployment := object(t, objects, "Deployment", "bulwark")
	if mapping(t, deployment["spec"])["replicas"] != 1 {
		t.Fatal("Bulwark keeps sessions per pod without a shared store, so it runs one replica")
	}
	pod := podSpec(t, objects)
	if pod["serviceAccountName"] != "bulwark" || pod["automountServiceAccountToken"] != false ||
		pod["hostNetwork"] == true || pod["hostPID"] == true || pod["hostIPC"] == true {
		t.Fatal("Bulwark must not gain host or Kubernetes API access")
	}
	podContext := mapping(t, pod["securityContext"])
	if podContext["runAsNonRoot"] != true || !reflect.DeepEqual(podContext["seccompProfile"], map[string]any{"type": "RuntimeDefault"}) {
		t.Fatal("the pod must run as non-root with the runtime's seccomp profile")
	}
	c := container(t, pod)
	context := mapping(t, c["securityContext"])
	if context["runAsNonRoot"] != true || context["readOnlyRootFilesystem"] != true || context["allowPrivilegeEscalation"] != false ||
		context["privileged"] != false || !reflect.DeepEqual(context["capabilities"], map[string]any{"drop": []any{"ALL"}}) {
		t.Fatal("the container must run restricted with a read-only root filesystem and no capabilities")
	}
	readiness := mapping(t, mapping(t, c["readinessProbe"])["httpGet"])
	timeout, _ := mapping(t, c["readinessProbe"])["timeoutSeconds"].(int)
	if readiness["path"] != "/api/health/ready" || timeout <= 2 {
		t.Fatal("readiness must include Stalwart's answer, which Bulwark waits up to 2 seconds for")
	}
	if mapping(t, mapping(t, c["livenessProbe"])["httpGet"])["path"] != "/api/health/live" {
		t.Fatal("liveness must not depend on Stalwart, or a mail outage would restart Bulwark")
	}
}

// Bulwark's server may reach only Stalwart's web side on 443, and the names it
// resolves on the way. Nothing may reach Bulwark yet: no ingress rule, no
// route. The swap step that serves it behind GitHub SSO changes this test.
func TestBulwarkReachesOnlyStalwartAndIsNotRoutedYet(t *testing.T) {
	objects := production(t)
	deny := mapping(t, object(t, objects, "NetworkPolicy", "default-deny")["spec"])
	if !reflect.DeepEqual(deny, map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}}) {
		t.Fatal("the namespace must deny all ingress and egress by default")
	}
	var policies []map[string]any
	for _, item := range objects {
		metadata, _ := item["metadata"].(map[string]any)
		if metadata["namespace"] == namespace && strings.HasSuffix(item["kind"].(string), "NetworkPolicy") && item["kind"] != "NetworkPolicy" {
			policies = append(policies, item)
		}
	}
	if len(policies) != 1 || policies[0]["kind"] != "CiliumNetworkPolicy" {
		t.Fatalf("Bulwark must have exactly one Cilium allow policy, got %d", len(policies))
	}
	spec := mapping(t, policies[0]["spec"])
	if !reflect.DeepEqual(spec["endpointSelector"], map[string]any{"matchLabels": map[string]any{"k8s:app.kubernetes.io/name": "bulwark", "k8s:app.kubernetes.io/instance": "bulwark"}}) {
		t.Fatal("the allow policy must select Bulwark's pods")
	}
	for _, key := range []string{"ingress", "ingressDeny", "egressDeny"} {
		if _, present := spec[key]; present {
			t.Fatalf("no %s rule is admitted before the route exists", key)
		}
	}
	stalwart := "stalwart.${domain}"
	wantEgress := []any{
		map[string]any{
			"toFQDNs": []any{map[string]any{"matchName": stalwart}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
		},
		map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "53", "protocol": "UDP"}, map[string]any{"port": "53", "protocol": "TCP"}},
				"rules": map[string]any{"dns": []any{map[string]any{"matchName": stalwart}, map[string]any{"matchPattern": "*.cluster.local"}}},
			}},
		},
	}
	if !reflect.DeepEqual(spec["egress"], wantEgress) {
		t.Fatalf("egress must be exactly Stalwart's web side on 443 and its DNS lookup, got %v", spec["egress"])
	}
	for _, item := range objects {
		kind, _ := item["kind"].(string)
		if !strings.HasSuffix(kind, "Route") && kind != "Ingress" && kind != "ReferenceGrant" {
			continue
		}
		metadata, _ := item["metadata"].(map[string]any)
		if metadata["namespace"] == namespace {
			t.Fatalf("%s %s: nothing routes to Bulwark before the SSO-gated swap", kind, metadata["name"])
		}
		encoded, err := json.Marshal(item["spec"])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"namespace":"bulwark"`) {
			t.Fatalf("%s %s/%s refers to the bulwark namespace", kind, metadata["namespace"], metadata["name"])
		}
	}
}

// A third-party app runs in production only (ADR 0016).
func TestBulwarkIsProductionOnly(t *testing.T) {
	for _, item := range render(t, "k8s/providers/docker/apps") {
		metadata, _ := item["metadata"].(map[string]any)
		if metadata["namespace"] == namespace || (item["kind"] == "Namespace" && metadata["name"] == namespace) {
			t.Fatal("Bulwark must not deploy to the local cluster")
		}
	}
}
