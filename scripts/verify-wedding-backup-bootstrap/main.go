package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
)

type object = map[string]any
type recipe struct{ push, pull, store, minio object }

var refused = errors.New("safety check failed")

func at(o object, keys ...string) any {
	var v any = o
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
func str(o object, keys ...string) string { s, _ := at(o, keys...).(string); return s }

const ownerKey = "platform.devantler.tech/bootstrap-proof-run"
const dedicatedPath = "apps/wedding-app/backup/r2"
const projectedSecret = "wedding-db-backup-r2-dedicated"
const baoImage = "quay.io/openbao/openbao:2.5.3@sha256:fdc6da21ca6963560c32336fd7feb9cf2d5e52668f1a1647205a4b41171f0806"
const mcImage = "quay.io/minio/aistor/mc:RELEASE.2026-03-12T04-18-55Z@sha256:6c33dc0fbf65c362be95003cd010ed95a41c556500833ea139f86de40c4c4e9f"
const toolsImage = "docker.io/library/busybox:1.38.0-musl@sha256:ea2b9914a16a4ac1981994af97b318f7c7d4db76b580c56177f08bf76f4a0be8"

// This reviewed startup tuple must match the local-provider recipe. The fixture
// refuses a configuration that could omit authentication or expose other APIs.
const s3ServerScript = `umask 077
access=$(cat /etc/minio-credentials/rootUser)
password=$(cat /etc/minio-credentials/rootPassword)
case "$access" in ''|*[!A-Za-z0-9_+=/-]*) exit 1;; esac
case "$password" in ''|*[!A-Za-z0-9_+=/-]*) exit 1;; esac
test "${#access}" -ge 3
test "${#access}" -le 64
test "${#password}" -ge 8
test "${#password}" -le 128
test "$(wc -c </etc/minio-credentials/rootUser)" -eq "${#access}"
test "$(wc -c </etc/minio-credentials/rootPassword)" -eq "${#password}"
printf '{"identities":[{"name":"fixture","credentials":[{"accessKey":"%s","secretKey":"%s"}],"actions":["Admin","Read","List","Tagging","Write"]}]}' "$access" "$password" >/tmp/s3.json
unset access password
exec /usr/bin/weed -logtostderr=true server -dir=/data -filer -s3 \
  -ip=127.0.0.1 -ip.bind=127.0.0.1 -s3.ip.bind=0.0.0.0 \
  -s3.port=9000 -s3.config=/tmp/s3.json -s3.iam=false \
  -s3.port.iceberg=0 -s3.port.lance=0 -master.telemetry=false \
  -master.volumeSizeLimitMB=64 -volume.max=4
`

var digits = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var uuid = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func readObject(filename string) (object, error) {
	a, err := filepath.Abs(filename)
	if err != nil {
		return nil, refused
	}
	real, err := filepath.EvalSymlinks(a)
	if err != nil || real != a {
		return nil, refused
	}
	s, err := os.Lstat(a)
	if err != nil || !s.Mode().IsRegular() || s.Size() < 1 || s.Size() > 262144 {
		return nil, refused
	}
	b, err := os.ReadFile(a)
	if err != nil {
		return nil, refused
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	var o object
	if d.Decode(&o) != nil || o == nil {
		return nil, refused
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, refused
	}
	return o, nil
}
func loadRecipe(root string) (recipe, error) {
	paths := []string{"k8s/bases/infrastructure/vault-seed/push-secret-seed-wedding-db-backup-r2.yaml", "k8s/bases/apps/wedding-app/external-secret-db-backup-dedicated.yaml", "k8s/bases/apps/wedding-app/object-store-dedicated.yaml", "k8s/providers/docker/infrastructure/controllers/minio/deployment.yaml"}
	items := make([]object, 4)
	for i, p := range paths {
		var err error
		items[i], err = readObject(filepath.Join(root, p))
		if err != nil {
			return recipe{}, err
		}
	}
	r := recipe{items[0], items[1], items[2], items[3]}
	return r, validateRecipe(r)
}
func validateRecipe(r recipe) error {
	if str(r.push, "kind") != "PushSecret" || str(r.push, "metadata", "name") != "seed-wedding-db-backup-r2" || str(r.push, "metadata", "namespace") != "flux-system" || str(r.push, "spec", "refreshInterval") != "1h" || str(r.push, "spec", "selector", "secret", "name") != "wedding-db-backup-r2-bootstrap" {
		return refused
	}
	wantPush := []any{}
	wantPull := []any{}
	for _, pair := range [][2]string{{"access_key_id", "ACCESS_KEY_ID"}, {"secret_access_key", "SECRET_ACCESS_KEY"}} {
		wantPush = append(wantPush, object{"match": object{"secretKey": pair[0], "remoteRef": object{"remoteKey": dedicatedPath, "property": pair[0]}}})
		wantPull = append(wantPull, object{"secretKey": pair[1], "remoteRef": object{"key": dedicatedPath, "property": pair[0]}})
	}
	if !reflect.DeepEqual(at(r.push, "spec", "data"), wantPush) || !reflect.DeepEqual(at(r.push, "spec", "secretStoreRefs"), []any{object{"name": "openbao", "kind": "ClusterSecretStore"}}) {
		return refused
	}
	if str(r.pull, "kind") != "ExternalSecret" || str(r.pull, "metadata", "namespace") != "wedding-app" || str(r.pull, "metadata", "name") != projectedSecret || str(r.pull, "spec", "refreshInterval") != "1h" || str(r.pull, "spec", "target", "name") != projectedSecret || str(r.pull, "spec", "target", "creationPolicy") != "Owner" || !reflect.DeepEqual(at(r.pull, "spec", "secretStoreRef"), object{"name": "openbao", "kind": "ClusterSecretStore"}) || !reflect.DeepEqual(at(r.pull, "spec", "data"), wantPull) {
		return refused
	}
	if !reflect.DeepEqual(at(r.pull, "spec", "target", "template", "data"), object{"ACCESS_KEY_ID": "{{ .ACCESS_KEY_ID }}", "SECRET_ACCESS_KEY": "{{ .SECRET_ACCESS_KEY }}", "REGION": "auto"}) {
		return refused
	}
	refs := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		refs[field] = object{"name": projectedSecret, "key": key}
	}
	if str(r.store, "kind") != "ObjectStore" || str(r.store, "metadata", "name") != "wedding-db-dedicated" || str(r.store, "spec", "configuration", "destinationPath") != "s3://wedding-db-backups/cnpg/wedding-db" || str(r.store, "spec", "configuration", "endpointURL") != "${r2_endpoint}" || !reflect.DeepEqual(at(r.store, "spec", "configuration", "s3Credentials"), refs) {
		return refused
	}
	containers, _ := at(r.minio, "spec", "template", "spec", "containers").([]any)
	if str(r.minio, "kind") != "Deployment" || len(containers) != 1 {
		return refused
	}
	c, ok := containers[0].(map[string]any)
	if !ok || !regexp.MustCompile(`^docker\.io/chrislusf/seaweedfs:[0-9]+\.[0-9]+@sha256:[a-f0-9]{64}$`).MatchString(str(c, "image")) {
		return refused
	}
	if at(c, "env") != nil || at(c, "envFrom") != nil || !reflect.DeepEqual(at(c, "command"), []any{"/bin/sh", "-ec"}) || !reflect.DeepEqual(at(c, "args"), []any{s3ServerScript}) {
		return refused
	}
	return nil
}
func copyObject(o object) object {
	b, _ := json.Marshal(o)
	var result object
	_ = json.Unmarshal(b, &result)
	return result
}
func meta(name, ns, run string) object {
	m := object{"name": name, "labels": object{ownerKey: run, "app.kubernetes.io/managed-by": "github-actions"}}
	if ns != "" {
		m["namespace"] = ns
	}
	return m
}
func fixture(run string, r recipe, access, password string) ([]object, error) {
	if !digits.MatchString(run) || validateRecipe(r) != nil || !regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`).MatchString(access) || !regexp.MustCompile(`^[A-Za-z0-9]{20,128}$`).MatchString(password) || access == password {
		return nil, refused
	}
	ns := "wedding-bootstrap-" + run
	nsm := meta(ns, "", run)
	nsm["labels"].(map[string]any)["pod-security.kubernetes.io/enforce"] = "restricted"
	security := object{"runAsNonRoot": true, "runAsUser": 100, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": object{"type": "RuntimeDefault"}}
	csecurity := object{"runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}
	config := `disable_mlock = true
storage "file" { path = "/openbao/data" }
listener "tcp" {
  address = "0.0.0.0:8200"
  tls_disable = true
}
api_addr = "http://openbao:8200"
ui = false
`
	minioSpec := copyObject(at(r.minio, "spec", "template", "spec").(map[string]any))
	for _, v := range minioSpec["volumes"].([]any) {
		volume := v.(map[string]any)
		if volume["emptyDir"] != nil {
			volume["emptyDir"] = object{"medium": "Memory", "sizeLimit": "128Mi"}
		}
	}
	minioLabels := object{"app.kubernetes.io/name": "minio", ownerKey: run}
	baoLabels := object{"app.kubernetes.io/name": "openbao", ownerKey: run}
	podmeta := func(name string, labels object) object { m := meta(name, ns, run); m["labels"] = labels; return m }
	push, pull := copyObject(r.push), copyObject(r.pull)
	push["metadata"] = meta("seed-wedding-db-backup-r2", ns, run)
	pull["metadata"] = meta(projectedSecret, ns, run)
	push["spec"].(map[string]any)["secretStoreRefs"] = []any{object{"name": "fixture-openbao", "kind": "SecretStore"}}
	push["spec"].(map[string]any)["refreshInterval"] = "5s"
	pull["spec"].(map[string]any)["secretStoreRef"] = object{"name": "fixture-openbao", "kind": "SecretStore"}
	pull["spec"].(map[string]any)["refreshInterval"] = "5s"
	pull["spec"].(map[string]any)["target"].(map[string]any)["template"].(map[string]any)["metadata"] = object{"labels": object{ownerKey: run}}
	apiPorts := []any{object{"ports": []any{object{"port": "9000", "protocol": "TCP"}, object{"port": "8200", "protocol": "TCP"}}}}
	return []object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": nsm},
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("openbao-config", ns, run), "data": object{"config.hcl": config}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("minio-root-credentials", ns, run), "type": "Opaque", "stringData": object{"rootUser": access, "rootPassword": password}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("wedding-db-backup-r2-bootstrap", ns, run), "type": "Opaque", "stringData": object{"access_key_id": access, "secret_access_key": password}},
		{"apiVersion": "v1", "kind": "Pod", "metadata": podmeta("openbao", baoLabels), "spec": object{"automountServiceAccountToken": false, "securityContext": security, "containers": []any{object{"name": "openbao", "image": baoImage, "command": []any{"bao", "server", "-config=/openbao/config/config.hcl"}, "securityContext": csecurity, "ports": []any{object{"containerPort": 8200}}, "readinessProbe": object{"tcpSocket": object{"port": 8200}, "periodSeconds": 2}, "resources": object{"requests": object{"cpu": "50m", "memory": "128Mi"}, "limits": object{"cpu": "500m", "memory": "512Mi"}}, "volumeMounts": []any{object{"name": "config", "mountPath": "/openbao/config", "readOnly": true}, object{"name": "data", "mountPath": "/openbao/data"}, object{"name": "tmp", "mountPath": "/tmp"}}}}, "volumes": []any{object{"name": "config", "configMap": object{"name": "openbao-config"}}, object{"name": "data", "emptyDir": object{"medium": "Memory", "sizeLimit": "64Mi"}}, object{"name": "tmp", "emptyDir": object{"medium": "Memory", "sizeLimit": "16Mi"}}}}},
		{"apiVersion": "v1", "kind": "Pod", "metadata": podmeta("minio", minioLabels), "spec": minioSpec},
		{"apiVersion": "v1", "kind": "Service", "metadata": meta("openbao", ns, run), "spec": object{"selector": baoLabels, "ports": []any{object{"port": 8200, "targetPort": 8200}}}},
		{"apiVersion": "v1", "kind": "Service", "metadata": meta("minio", ns, run), "spec": object{"selector": minioLabels, "ports": []any{object{"port": 9000, "targetPort": 9000}}}},
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": meta("fixture-isolation", ns, run), "spec": object{"endpointSelector": object{}, "ingress": []any{object{"fromEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": ns}}, object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "external-secrets"}}}, "toPorts": apiPorts}, object{"fromEntities": []any{"host", "remote-node", "kube-apiserver"}, "toPorts": apiPorts}}, "egress": []any{object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": ns}}}}, object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}}, "toPorts": []any{object{"ports": []any{object{"port": "53", "protocol": "UDP"}, object{"port": "53", "protocol": "TCP"}}}}}}}},
		push, pull,
	}, nil
}
func invocation(env func(string) string) (string, error) {
	run := env("GITHUB_RUN_ID")
	if env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || env("WEDDING_BOOTSTRAP_CONFIRM") != "verify-wedding-backup-bootstrap" || !digits.MatchString(run) {
		return "", refused
	}
	return run, nil
}
