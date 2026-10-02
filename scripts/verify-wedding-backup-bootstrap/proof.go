package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net"
	"reflect"
	"strings"
)

func projection(s, es object, run, access, password string) bool {
	if !owned(s, projectedSecret, "wedding-bootstrap-"+run, run) {
		return false
	}
	refs, _ := at(s, "metadata", "ownerReferences").([]any)
	if len(refs) != 1 {
		return false
	}
	owner, ok := refs[0].(map[string]any)
	if !ok || str(owner, "kind") != "ExternalSecret" || str(owner, "name") != projectedSecret || str(owner, "uid") != str(es, "metadata", "uid") || owner["controller"] != true {
		return false
	}
	data, ok := s["data"].(map[string]any)
	if !ok || len(data) != 3 {
		return false
	}
	for key, want := range map[string]string{"ACCESS_KEY_ID": access, "SECRET_ACCESS_KEY": password, "REGION": "auto"} {
		value, err := base64.StdEncoding.DecodeString(str(data, key))
		if err != nil || string(value) != want {
			return false
		}
	}
	return true
}

func probePod(run string, r recipe) (object, error) {
	if !digits.MatchString(run) || validateRecipe(r) != nil {
		return nil, refused
	}
	destination := strings.TrimPrefix(str(r.store, "spec", "configuration", "destinationPath"), "s3://")
	script := `set -eu
umask 077
access=$(cat /credentials/ACCESS_KEY_ID)
password=$(cat /credentials/SECRET_ACCESS_KEY)
test "$(cat /credentials/REGION)" = auto
# Test the Pod IP directly: the Service publishes only 9000 and would hide an
# unrestricted management listener even if the network policy were missing.
nc -z -w 5 "${S3_POD_IP:?}" 9000 >/dev/null 2>&1
if nc -z -w 3 "$S3_POD_IP" 19000 >/dev/null 2>&1; then exit 1; fi
printf '{"url":"http://minio:9000","accessKey":"%s","secretKey":"%s","api":"S3v4","path":"on"}' "$access" "$password" >/tmp/alias.json
mc --config-dir /tmp/mc alias import local /tmp/alias.json >/dev/null 2>&1
unset access password
mc --config-dir /tmp/mc mb --ignore-existing local/wedding-db-backups >/dev/null 2>&1
printf '%s' 'wedding-bootstrap-` + run + `' >/tmp/sentinel
mc --config-dir /tmp/mc cp /tmp/sentinel local/` + destination + `/bootstrap-proof-` + run + ` >/dev/null 2>&1
mc --config-dir /tmp/mc cat local/` + destination + `/bootstrap-proof-` + run + ` >/tmp/readback 2>/dev/null
cmp /tmp/sentinel /tmp/readback
`
	return object{"apiVersion": "v1", "kind": "Pod", "metadata": meta("storage-proof", "wedding-bootstrap-"+run, run), "spec": object{
		"restartPolicy": "Never", "activeDeadlineSeconds": 120, "automountServiceAccountToken": false,
		"securityContext": object{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": object{"type": "RuntimeDefault"}},
		"initContainers":  []any{object{"name": "install-tools", "image": toolsImage, "command": []any{"/bin/sh", "-ec", "cp /bin/busybox /tools/busybox; /tools/busybox --install -s /tools"}, "securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}, "resources": object{"requests": object{"cpu": "10m", "memory": "8Mi"}, "limits": object{"cpu": "100m", "memory": "32Mi"}}, "volumeMounts": []any{object{"name": "tools", "mountPath": "/tools"}}}},
		"containers":      []any{object{"name": "proof", "image": mcImage, "command": []any{"/tools/sh", "-c", script}, "env": []any{object{"name": "PATH", "value": "/tools:/usr/local/bin:/usr/bin:/bin"}}, "securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}, "resources": object{"requests": object{"cpu": "10m", "memory": "32Mi"}, "limits": object{"cpu": "200m", "memory": "128Mi"}}, "volumeMounts": []any{object{"name": "credentials", "mountPath": "/credentials", "readOnly": true}, object{"name": "tmp", "mountPath": "/tmp"}, object{"name": "tools", "mountPath": "/tools", "readOnly": true}}}},
		"volumes":         []any{object{"name": "credentials", "secret": object{"secretName": projectedSecret, "defaultMode": 0440}}, object{"name": "tmp", "emptyDir": object{"medium": "Memory", "sizeLimit": "16Mi"}}, object{"name": "tools", "emptyDir": object{"medium": "Memory", "sizeLimit": "8Mi"}}},
	}}, nil
}

func randomCredential(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", refused
	}
	return hex.EncodeToString(b), nil
}

func bindPeerAddress(p, server object, run string) error {
	ip := net.ParseIP(str(server, "status", "podIP"))
	if !owned(server, "minio", "wedding-bootstrap-"+run, run) || ip == nil || !ip.IsGlobalUnicast() {
		return refused
	}
	c := at(p, "spec", "containers").([]any)[0].(map[string]any)
	c["env"] = append(at(c, "env").([]any), object{"name": "S3_POD_IP", "value": ip.String()})
	return nil
}

func unchangedSource(before, after object) bool {
	return uuid.MatchString(str(before, "metadata", "uid")) && str(before, "metadata", "uid") == str(after, "metadata", "uid") && str(before, "metadata", "resourceVersion") != "" && str(before, "metadata", "resourceVersion") == str(after, "metadata", "resourceVersion") && reflect.DeepEqual(before["data"], after["data"])
}
