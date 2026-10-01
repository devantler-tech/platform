package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type object = map[string]any
type source struct {
	UID, StoreUID, Image, Size, Host, BackupID string
	Generation, StoreGeneration                int
}
type inventory struct {
	Pairs    int    `json:"pairs"`
	Guests   int    `json:"guests"`
	Answers  int    `json:"answers"`
	Bookings int    `json:"bookings"`
	Latest   string `json:"latest"`
}

var refused = errors.New("safety check failed")
var digits = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var uuid = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var endpoint = regexp.MustCompile(`^https://([0-9a-f]{32}\.r2\.cloudflarestorage\.com)$`)

const credential = "wedding-db-backup-r2-dedicated"
const ownerKey = "platform.devantler.tech/restore-proof-run"

func value(o object, path ...string) any {
	var v any = o
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
func str(o object, path ...string) string { v, _ := value(o, path...).(string); return v }
func number(o object, path ...string) int { v, _ := value(o, path...).(float64); return int(v) }
func ready(o object, kind string) bool {
	conditions, _ := value(o, "status", "conditions").([]any)
	for _, item := range conditions {
		c, _ := item.(map[string]any)
		if c["type"] == kind && c["status"] == "True" {
			return true
		}
	}
	return false
}
func validateSource(c, s, b object) (source, error) {
	uid := str(c, "metadata", "uid")
	if str(c, "metadata", "name") != "wedding-db" || str(c, "metadata", "namespace") != "wedding-app" || !uuid.MatchString(uid) || value(c, "metadata", "deletionTimestamp") != nil || number(c, "metadata", "generation") < 1 || number(c, "spec", "instances") != 3 || number(c, "status", "readyInstances") != 3 || !ready(c, "Ready") || !ready(c, "ContinuousArchiving") {
		return source{}, refused
	}
	p, _ := value(c, "spec", "plugins").([]any)
	if len(p) != 1 {
		return source{}, refused
	}
	plugin, _ := p[0].(map[string]any)
	if plugin["name"] != "barman-cloud.cloudnative-pg.io" || plugin["enabled"] != true || plugin["isWALArchiver"] != true || str(plugin, "parameters", "barmanObjectName") != "wedding-db-dedicated" || str(plugin, "parameters", "serverName") != "wedding-db-20260909" {
		return source{}, refused
	}
	if str(s, "metadata", "name") != "wedding-db-dedicated" || str(s, "metadata", "namespace") != "wedding-app" || !uuid.MatchString(str(s, "metadata", "uid")) || number(s, "metadata", "generation") < 1 || value(s, "metadata", "deletionTimestamp") != nil || str(s, "spec", "configuration", "destinationPath") != "s3://wedding-db-backups/cnpg/wedding-db" {
		return source{}, refused
	}
	host := endpoint.FindStringSubmatch(str(s, "spec", "configuration", "endpointURL"))
	if len(host) != 2 {
		return source{}, refused
	}
	want := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		want[field] = object{"name": credential, "key": key}
	}
	if !reflect.DeepEqual(value(s, "spec", "configuration", "s3Credentials"), want) {
		return source{}, refused
	}
	if str(b, "metadata", "namespace") != "wedding-app" || str(b, "status", "phase") != "completed" || str(b, "spec", "cluster", "name") != "wedding-db" || str(b, "spec", "method") != "plugin" || str(b, "spec", "pluginConfiguration", "name") != "barman-cloud.cloudnative-pg.io" || str(b, "status", "pluginMetadata", "clusterUID") != uid || str(b, "status", "pluginMetadata", "name") != "barman-cloud.cloudnative-pg.io" || !regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`).MatchString(str(b, "status", "backupId")) {
		return source{}, refused
	}
	image, size := str(c, "spec", "imageName"), str(c, "spec", "storage", "size")
	if !regexp.MustCompile(`^ghcr\.io/cloudnative-pg/postgresql:18\.[0-9]+-[a-z0-9-]+$`).MatchString(image) || !regexp.MustCompile(`^[1-9][0-9]*(Mi|Gi)$`).MatchString(size) || str(c, "spec", "storage", "storageClass") != "longhorn-wffc" {
		return source{}, refused
	}
	return source{uid, str(s, "metadata", "uid"), image, size, host[1], str(b, "status", "backupId"), number(c, "metadata", "generation"), number(s, "metadata", "generation")}, nil
}

func metadata(name, namespace, run string) object {
	return object{"name": name, "namespace": namespace, "labels": object{ownerKey: run, "app.kubernetes.io/managed-by": "github-actions"}}
}
func resources(run string, s source) []object {
	ns := "wedding-restore-" + run
	meta := metadata(ns, "", run)
	delete(meta, "namespace")
	meta["labels"].(map[string]any)["pod-security.kubernetes.io/enforce"] = "restricted"
	refs := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		refs[field] = object{"name": credential, "key": key}
	}
	return []object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": meta},
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": metadata("restore-isolation", ns, run), "spec": object{
			"endpointSelector": object{}, "ingress": []any{},
			"egressDeny": []any{object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "wedding-app"}}}}},
			"egress": []any{
				object{"toEntities": []any{"kube-apiserver"}},
				// Intercept DNS on cold nodes so Cilium can learn the R2 IPs.
				object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}}, "toPorts": []any{object{"ports": []any{object{"port": "53", "protocol": "UDP"}, object{"port": "53", "protocol": "TCP"}}, "rules": object{"dns": []any{object{"matchPattern": "*"}}}}}},
				object{"toFQDNs": []any{object{"matchName": s.Host}}, "toPorts": []any{object{"ports": []any{object{"port": "443", "protocol": "TCP"}}}}},
			},
		}},
		{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": metadata("dedicated-restore", ns, run), "spec": object{"configuration": object{"destinationPath": "s3://wedding-db-backups/cnpg/wedding-db", "endpointURL": "https://" + s.Host, "s3Credentials": refs, "wal": object{"compression": "gzip"}, "data": object{"compression": "gzip"}}}},
		{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": metadata("restore", ns, run), "spec": object{
			"instances": 1, "imageName": s.Image, "enableSuperuserAccess": false, "enablePDB": false,
			"storage":          object{"size": s.Size, "storageClass": "longhorn-wffc"},
			"resources":        object{"requests": object{"cpu": "50m", "memory": "256Mi"}, "limits": object{"cpu": "1", "memory": "1Gi"}},
			"bootstrap":        object{"recovery": object{"source": "dedicated", "recoveryTarget": object{"backupID": s.BackupID}}},
			"externalClusters": []any{object{"name": "dedicated", "plugin": object{"name": "barman-cloud.cloudnative-pg.io", "parameters": object{"barmanObjectName": "dedicated-restore", "serverName": "wedding-db-20260909"}}}},
		}},
	}
}

// Counts must match an observed live boundary. Only timestamps may lag by the
// explicitly documented five-minute allowance for concurrently committed writes.
func compare(before, recovered, after inventory) error {
	if before.Pairs < 1 || before.Guests < 1 || recovered.Pairs != before.Pairs || recovered.Pairs != after.Pairs || recovered.Guests != before.Guests || recovered.Guests != after.Guests || recovered.Answers < 1 {
		return refused
	}
	for _, triple := range [][3]int{{before.Answers, recovered.Answers, after.Answers}, {before.Bookings, recovered.Bookings, after.Bookings}} {
		if triple[1] < min(triple[0], triple[2]) || triple[1] > max(triple[0], triple[2]) {
			return refused
		}
	}
	b, eb := time.Parse(time.RFC3339Nano, before.Latest)
	r, er := time.Parse(time.RFC3339Nano, recovered.Latest)
	a, ea := time.Parse(time.RFC3339Nano, after.Latest)
	if eb != nil || er != nil || ea != nil || r.After(a) || b.Sub(r) > 5*time.Minute {
		return refused
	}
	return nil
}

type client struct {
	ctx     context.Context
	command func(context.Context, []string, []byte) ([]byte, error)
}

func (k client) call(ns string, input []byte, args ...string) ([]byte, error) {
	prefix := []string{"--context", "admin@prod", "--request-timeout=30s"}
	if ns != "" {
		prefix = append(prefix, "--namespace", ns)
	}
	return k.command(k.ctx, append(prefix, args...), input)
}
func (k client) get(ns, kind, name string) (object, error) {
	b, err := k.call(ns, nil, "get", kind, name, "--ignore-not-found=true", "-o", "json")
	if err != nil {
		return nil, refused
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var o object
	if json.Unmarshal(b, &o) != nil || o == nil {
		return nil, refused
	}
	return o, nil
}
func (k client) create(o object) error {
	b, err := json.Marshal(o)
	if err != nil {
		return refused
	}
	_, err = k.call("", b, "create", "-f", "-", "-o", "json")
	return err
}
func (k client) list(ns, kind string) ([]object, error) {
	b, err := k.call(ns, nil, "get", kind, "-o", "json")
	if err != nil {
		return nil, refused
	}
	var list struct {
		Items []object `json:"items"`
	}
	if json.Unmarshal(b, &list) != nil || list.Items == nil {
		return nil, refused
	}
	return list.Items, nil
}

const inventorySQL = `BEGIN READ ONLY; SELECT json_build_object('pairs',(SELECT count(*) FROM guest_pairs),'guests',(SELECT count(*) FROM guests),'answers',(SELECT count(*) FROM guests WHERE attending IS NOT NULL OR dietary_notes IS NOT NULL),'bookings',(SELECT count(*) FROM room_bookings),'latest',to_char((SELECT max(t) FROM (SELECT max(created_at) AS t FROM guest_pairs UNION ALL SELECT max(updated_at) FROM guests UNION ALL SELECT max(updated_at) FROM room_bookings) newest) AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')); COMMIT;`

func (k client) inventory(ns, primary string) (inventory, error) {
	if !regexp.MustCompile(`^(wedding-db|restore)-[1-9][0-9]*$`).MatchString(primary) {
		return inventory{}, refused
	}
	b, err := k.call(ns, nil, "exec", primary, "--container=postgres", "--", "psql", "--username=postgres", "--dbname=wedding", "--set=ON_ERROR_STOP=1", "--tuples-only", "--no-align", "--quiet", "--command="+inventorySQL)
	var result inventory
	if err != nil || json.Unmarshal(bytes.TrimSpace(b), &result) != nil {
		return inventory{}, refused
	}
	return result, nil
}
func owns(o object, run string) bool {
	return o != nil && str(o, "metadata", "name") == "wedding-restore-"+run && str(o, "metadata", "labels", ownerKey) == run && uuid.MatchString(str(o, "metadata", "uid"))
}
func cleanup(k client, run, expectedUID string) error {
	ns := "wedding-restore-" + run
	o, err := k.get("", "namespaces", ns)
	if err != nil {
		return err
	}
	if o != nil {
		if !owns(o, run) || (expectedUID != "" && str(o, "metadata", "uid") != expectedUID) {
			return refused
		}
		// Raw DELETE carries a UID precondition; a same-name replacement is safe.
		body, _ := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": str(o, "metadata", "uid")}, "propagationPolicy": "Foreground"})
		if _, err = k.call("", body, "delete", "--raw=/api/v1/namespaces/"+ns, "-f", "-"); err != nil {
			return refused
		}
		if _, err = k.call("", nil, "wait", "--for=delete", "namespace/"+ns, "--timeout=10m"); err != nil {
			return refused
		}
	}
	// Namespace deletion includes the temporary policies, Secret, Cluster and
	// PVC. Wait for the storage controller to reclaim every backing PV as well.
	for attempt := 0; attempt < 120; attempt++ {
		volumes, err := k.list("", "persistentvolumes")
		if err != nil {
			return err
		}
		remaining := false
		for _, pv := range volumes {
			if str(pv, "spec", "claimRef", "namespace") == ns {
				remaining = true
				if str(pv, "spec", "persistentVolumeReclaimPolicy") != "Delete" {
					return refused
				}
			}
		}
		if !remaining {
			remainingNS, err := k.get("", "namespaces", ns)
			if err != nil || remainingNS != nil {
				return refused
			}
			return nil
		}
		select {
		case <-k.ctx.Done():
			return refused
		case <-time.After(5 * time.Second):
		}
	}
	return refused
}

func prove(k client, run string) (result object, err error) {
	ns := "wedding-restore-" + run
	existing, err := k.get("", "namespaces", ns)
	if err != nil || existing != nil {
		return nil, refused
	}
	c, err := k.get("wedding-app", "clusters.postgresql.cnpg.io", "wedding-db")
	if err != nil {
		return nil, err
	}
	s, err := k.get("wedding-app", "objectstores.barmancloud.cnpg.io", "wedding-db-dedicated")
	if err != nil {
		return nil, err
	}
	b, err := k.get("wedding-app", "backups.postgresql.cnpg.io", "wedding-db-dedicated-proof-"+run)
	if err != nil {
		return nil, err
	}
	state, err := validateSource(c, s, b)
	if err != nil {
		return nil, err
	}
	before, err := k.inventory("wedding-app", str(c, "status", "currentPrimary"))
	if err != nil {
		return nil, err
	}
	// Flush the current segment so the restore can observe recent committed data.
	if _, err = k.call("wedding-app", nil, "exec", str(c, "status", "currentPrimary"), "--container=postgres", "--", "psql", "--username=postgres", "--dbname=postgres", "--set=ON_ERROR_STOP=1", "--quiet", "--command=SELECT pg_switch_wal();"); err != nil {
		return nil, refused
	}
	items := resources(run, state)
	if err = k.create(items[0]); err != nil {
		return nil, refused
	}
	created, err := k.get("", "namespaces", ns)
	if err != nil || !owns(created, run) {
		return nil, refused
	}
	nsUID := str(created, "metadata", "uid")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		cleanupClient := k
		cleanupClient.ctx = ctx
		if cleanup(cleanupClient, run, nsUID) != nil {
			result = nil
			err = errors.New("cleanup verification failed")
		}
	}()
	if err = k.create(items[1]); err != nil {
		return nil, refused
	}
	secrets, err := k.list(ns, "secrets")
	if err != nil || len(secrets) != 0 {
		return nil, refused
	}
	secret, err := k.get("wedding-app", "secrets", credential)
	if err != nil {
		return nil, refused
	}
	data, _ := value(secret, "data").(map[string]any)
	if len(data) != 3 || str(secret, "metadata", "name") != credential {
		return nil, refused
	}
	for _, key := range []string{"ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "REGION"} {
		if v, ok := data[key].(string); !ok || v == "" {
			return nil, refused
		}
	}
	if err = k.create(object{"apiVersion": "v1", "kind": "Secret", "metadata": metadata(credential, ns, run), "type": "Opaque", "data": data}); err != nil {
		return nil, refused
	}
	// No value or Secret body reaches stdout, stderr or the proof receipt.
	for _, item := range items[2:] {
		if err = k.create(item); err != nil {
			return nil, refused
		}
	}
	if _, err = k.call(ns, nil, "wait", "--for=condition=Ready", "cluster.postgresql.cnpg.io/restore", "--timeout=30m"); err != nil {
		return nil, refused
	}
	restored, err := k.get(ns, "clusters.postgresql.cnpg.io", "restore")
	if err != nil || number(restored, "status", "readyInstances") != 1 || !ready(restored, "Ready") || value(restored, "spec", "plugins") != nil {
		return nil, refused
	}
	// Resolve the live service first and prove the probe works locally. Only
	// PostgreSQL's no-response result satisfies the negative connectivity check.
	const isolationProbe = `set -eu; pg_isready -h 127.0.0.1 -p 5432 -t 3 >/dev/null; getent ahostsv4 wedding-db-rw.wedding-app.svc.cluster.local >/dev/null; set +e; pg_isready -h wedding-db-rw.wedding-app.svc.cluster.local -p 5432 -t 3 >/dev/null; rc=$?; test "$rc" -eq 2`
	if _, err = k.call(ns, nil, "exec", str(restored, "status", "currentPrimary"), "--container=postgres", "--", "sh", "-c", isolationProbe); err != nil {
		return nil, refused
	}
	recovered, err := k.inventory(ns, str(restored, "status", "currentPrimary"))
	if err != nil {
		return nil, err
	}
	cAfter, err := k.get("wedding-app", "clusters.postgresql.cnpg.io", "wedding-db")
	if err != nil {
		return nil, err
	}
	sAfter, err := k.get("wedding-app", "objectstores.barmancloud.cnpg.io", "wedding-db-dedicated")
	if err != nil {
		return nil, err
	}
	stateAfter, err := validateSource(cAfter, sAfter, b)
	if err != nil || state != stateAfter {
		return nil, refused
	}
	after, err := k.inventory("wedding-app", str(cAfter, "status", "currentPrimary"))
	if err != nil {
		return nil, err
	}
	if err = compare(before, recovered, after); err != nil {
		return nil, err
	}
	return object{"dedicatedRestoreVerified": true, "productionClusterStable": true, "cleanupVerified": true, "backupID": state.BackupID, "recovered": recovered, "timestampToleranceSeconds": 300}, nil
}

func invocation(env func(string) string) (string, error) {
	run := env("GITHUB_RUN_ID")
	if env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || env("WEDDING_RESTORE_CONFIRM") != "verify-wedding-dedicated-restore" || !digits.MatchString(run) {
		return "", refused
	}
	return run, nil
}
func main() {
	stage := "invocation"
	err := func() error {
		run, err := invocation(os.Getenv)
		if err != nil {
			return err
		}
		mode := "prove"
		if len(os.Args) == 2 && os.Args[1] == "--cleanup" {
			mode = "cleanup"
		} else if len(os.Args) != 1 {
			return refused
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		k := client{ctx: ctx, command: func(ctx context.Context, args []string, input []byte) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "kubectl", args...)
			cmd.Stdin = bytes.NewReader(input)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
			// stderr may contain source content, endpoint details or credentials.
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil || len(out) > 4*1024*1024 {
				return nil, refused
			}
			return out, nil
		}}
		stage = mode
		if mode == "cleanup" {
			if err = cleanup(k, run, ""); err != nil {
				return err
			}
			fmt.Println(`{"cleanupVerified":true}`)
			return nil
		}
		result, err := prove(k, run)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Wedding dedicated restore refused (phase: "+strings.ReplaceAll(stage, "\n", "")+").")
		os.Exit(1)
	}
}
