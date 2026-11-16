// Command verify-portal-db-restore proves that a fresh online backup of the
// business-site portal database restores through its Barman Cloud plugin
// ObjectStore into a separate namespace that cannot reach production. The live
// database keeps serving throughout and is never restored over.
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

// source is what the restore plan copies from the live database. Any change to
// it while the proof runs refuses the proof.
type source struct {
	UID, StoreUID, Image, Size, Host string
	Generation, StoreGeneration      int
}

// receipt is the completed fresh backup the restore must recover.
type receipt struct {
	Name, BackupID, EndWAL string
}

// inventory holds aggregates only: the database owner, the migration ledger's
// size and highest version, and each table's row count. No row content leaves
// the database.
type inventory struct {
	Owner   string           `json:"databaseOwner"`
	Ledger  bool             `json:"ledgerPresent"`
	Applied int64            `json:"appliedMigrations"`
	Highest int64            `json:"highestVersion"`
	Tables  map[string]int64 `json:"tables"`
}

var errRefused = errors.New("safety check failed")
var digits = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var commit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var uuid = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var endpoint = regexp.MustCompile(`^https://([0-9a-f]{32}\.r2\.cloudflarestorage\.com)$`)
var backupIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`)
var walPattern = regexp.MustCompile(`^[0-9A-F]{24}$`)
var imagePattern = regexp.MustCompile(`^ghcr\.io/cloudnative-pg/postgresql:18\.[0-9]+-[a-z0-9-]+$`)
var sizePattern = regexp.MustCompile(`^[1-9][0-9]*(Mi|Gi)$`)

const (
	sourceNamespace = "business-site"
	clusterName     = "business-site-portal-db"
	storeName       = "business-site-portal-db"
	credential      = "business-site-portal-db-backup-r2"
	destination     = "s3://platform-backups/cnpg/business-site-portal-db"
	plugin          = "barman-cloud.cloudnative-pg.io"
	databaseName    = "portal"
	databaseOwner   = "portal_migrator"
	ledgerTable     = "portal_schema_version"
	// The live claims are hcloud volumes, which an admission policy refuses
	// on any Pod not pinned to the baseline workers. The restore copies both.
	storageClass  = "hcloud"
	baselineLabel = "platform.devantler.tech/baseline-worker"
	priorityClass = "hcloud-volume-workload"
	ownerKey      = "platform.devantler.tech/restore-proof-run"
	backupPrefix  = "business-site-portal-db-restore-proof-"
	restorePrefix = "portal-db-restore-"
)

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
func credentialRefs(name string) object {
	refs := object{}
	for field, key := range map[string]string{"accessKeyId": "ACCESS_KEY_ID", "secretAccessKey": "SECRET_ACCESS_KEY", "region": "REGION"} {
		refs[field] = object{"name": name, "key": key}
	}
	return refs
}

// validateSource accepts only the healthy, archiving portal database and the
// one ObjectStore it archives to, and returns what the restore plan copies.
func validateSource(c, s object) (source, error) {
	uid := str(c, "metadata", "uid")
	instances := number(c, "spec", "instances")
	if str(c, "metadata", "name") != clusterName || str(c, "metadata", "namespace") != sourceNamespace || !uuid.MatchString(uid) || value(c, "metadata", "deletionTimestamp") != nil || number(c, "metadata", "generation") < 1 || instances < 1 || number(c, "status", "readyInstances") != instances || !ready(c, "Ready") || !ready(c, "ContinuousArchiving") {
		return source{}, errRefused
	}
	if str(c, "spec", "bootstrap", "initdb", "database") != databaseName || str(c, "spec", "bootstrap", "initdb", "owner") != databaseOwner {
		return source{}, errRefused
	}
	p, _ := value(c, "spec", "plugins").([]any)
	if len(p) != 1 {
		return source{}, errRefused
	}
	// No serverName parameter: the archive's server name is the cluster name.
	entry, _ := p[0].(map[string]any)
	if entry["name"] != plugin || entry["enabled"] != true || entry["isWALArchiver"] != true || !reflect.DeepEqual(entry["parameters"], object{"barmanObjectName": storeName}) {
		return source{}, errRefused
	}
	if str(s, "metadata", "name") != storeName || str(s, "metadata", "namespace") != sourceNamespace || !uuid.MatchString(str(s, "metadata", "uid")) || number(s, "metadata", "generation") < 1 || value(s, "metadata", "deletionTimestamp") != nil || str(s, "spec", "configuration", "destinationPath") != destination {
		return source{}, errRefused
	}
	host := endpoint.FindStringSubmatch(str(s, "spec", "configuration", "endpointURL"))
	if len(host) != 2 || !reflect.DeepEqual(value(s, "spec", "configuration", "s3Credentials"), credentialRefs(credential)) {
		return source{}, errRefused
	}
	image, size := str(c, "spec", "imageName"), str(c, "spec", "storage", "size")
	if !imagePattern.MatchString(image) || !sizePattern.MatchString(size) || str(c, "spec", "storage", "storageClass") != storageClass || str(c, "spec", "affinity", "nodeSelector", baselineLabel) != "true" {
		return source{}, errRefused
	}
	return source{uid, str(s, "metadata", "uid"), image, size, host[1], number(c, "metadata", "generation"), number(s, "metadata", "generation")}, nil
}

func backupRequest(run string) object {
	return object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup", "metadata": metadata(backupPrefix+run, sourceNamespace, run), "spec": backupSpec()}
}
func backupSpec() object {
	return object{"cluster": object{"name": clusterName}, "method": "plugin", "pluginConfiguration": object{"name": plugin}, "target": "primary"}
}

// validateBackup accepts only this run's own completed plugin backup of the
// live cluster, taken after the request was created.
func validateBackup(b object, run, uid, clusterUID string) (receipt, error) {
	if str(b, "metadata", "name") != backupPrefix+run || str(b, "metadata", "namespace") != sourceNamespace || str(b, "metadata", "uid") != uid || str(b, "metadata", "labels", ownerKey) != run || value(b, "metadata", "deletionTimestamp") != nil || !reflect.DeepEqual(value(b, "spec"), backupSpec()) {
		return receipt{}, errRefused
	}
	if str(b, "status", "phase") != "completed" || str(b, "status", "method") != "plugin" || str(b, "status", "pluginMetadata", "clusterUID") != clusterUID || str(b, "status", "pluginMetadata", "name") != plugin || !backupIDPattern.MatchString(str(b, "status", "backupId")) || !walPattern.MatchString(str(b, "status", "endWal")) {
		return receipt{}, errRefused
	}
	created, ec := time.Parse(time.RFC3339, str(b, "metadata", "creationTimestamp"))
	started, es := time.Parse(time.RFC3339, str(b, "status", "startedAt"))
	stopped, ep := time.Parse(time.RFC3339, str(b, "status", "stoppedAt"))
	if ec != nil || es != nil || ep != nil || started.Before(created) || stopped.Before(started) {
		return receipt{}, errRefused
	}
	return receipt{backupPrefix + run, str(b, "status", "backupId"), str(b, "status", "endWal")}, nil
}

func metadata(name, namespace, run string) object {
	return object{"name": name, "namespace": namespace, "labels": object{ownerKey: run, "app.kubernetes.io/managed-by": "github-actions"}}
}
func resources(run string, s source, backupID string) []object {
	ns := restorePrefix + run
	meta := metadata(ns, "", run)
	delete(meta, "namespace")
	meta["labels"].(map[string]any)["pod-security.kubernetes.io/enforce"] = "restricted"
	return []object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": meta},
		{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": metadata("restore-isolation", ns, run), "spec": object{
			"endpointSelector": object{}, "ingress": []any{},
			"egressDeny": []any{object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": sourceNamespace}}}}},
			"egress": []any{
				object{"toEntities": []any{"kube-apiserver"}},
				// Intercept DNS on cold nodes so Cilium can learn the R2 IPs.
				object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}}, "toPorts": []any{object{"ports": []any{object{"port": "53", "protocol": "UDP"}, object{"port": "53", "protocol": "TCP"}}, "rules": object{"dns": []any{object{"matchPattern": "*"}}}}}},
				object{"toFQDNs": []any{object{"matchName": s.Host}}, "toPorts": []any{object{"ports": []any{object{"port": "443", "protocol": "TCP"}}}}},
			},
		}},
		// Read-only use of the live archive: no retention policy, and the
		// restored Cluster names no WAL archiver, so nothing writes to it.
		{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": metadata("portal-db-restore", ns, run), "spec": object{"configuration": object{"destinationPath": destination, "endpointURL": "https://" + s.Host, "s3Credentials": credentialRefs(credential), "wal": object{"compression": "gzip"}, "data": object{"compression": "gzip"}}}},
		{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": metadata("restore", ns, run), "spec": object{
			"instances": 1, "imageName": s.Image, "enableSuperuserAccess": false, "enablePDB": false,
			"priorityClassName": priorityClass,
			"affinity":          object{"nodeSelector": object{baselineLabel: "true"}},
			"storage":           object{"size": s.Size, "storageClass": storageClass},
			"resources":         object{"requests": object{"cpu": "50m", "memory": "256Mi"}, "limits": object{"cpu": "1", "memory": "1Gi"}},
			"bootstrap":         object{"recovery": object{"source": "portal", "recoveryTarget": object{"backupID": backupID}}},
			"externalClusters":  []any{object{"name": "portal", "plugin": object{"name": plugin, "parameters": object{"barmanObjectName": "portal-db-restore", "serverName": clusterName}}}},
		}},
	}
}

// checkInventory refuses a snapshot that is not the portal database or whose
// ledger is not the contiguous run of versions the migrations record.
func checkInventory(i inventory) bool {
	if i.Owner != databaseOwner || i.Tables == nil || i.Applied < 0 || i.Highest != i.Applied {
		return false
	}
	if !i.Ledger {
		return i.Applied == 0
	}
	rows, ok := i.Tables[ledgerTable]
	return ok && rows == i.Applied
}

// compare requires the recovered ledger to equal both live snapshots, the same
// tables everywhere, and each recovered row count to lie between the two live
// counts, since the portal may write while the proof runs. It reports whether
// the ledger is empty: the migration Job has not run, so only the database
// owner and an empty table set could be compared.
func compare(before, recovered, after inventory) (bool, error) {
	for _, i := range []inventory{before, recovered, after} {
		if !checkInventory(i) {
			return false, errRefused
		}
	}
	ledger := func(i inventory) [3]int64 {
		present := int64(0)
		if i.Ledger {
			present = 1
		}
		return [3]int64{present, i.Applied, i.Highest}
	}
	if ledger(recovered) != ledger(before) || ledger(recovered) != ledger(after) {
		return false, errRefused
	}
	if len(recovered.Tables) != len(before.Tables) || len(recovered.Tables) != len(after.Tables) {
		return false, errRefused
	}
	for table, rows := range recovered.Tables {
		b, inBefore := before.Tables[table]
		a, inAfter := after.Tables[table]
		if !inBefore || !inAfter || rows < min(b, a) || rows > max(b, a) {
			return false, errRefused
		}
	}
	return !recovered.Ledger || recovered.Applied == 0, nil
}

type client struct {
	ctx      context.Context
	interval time.Duration
	command  func(context.Context, []string, []byte) ([]byte, error)
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
		return nil, errRefused
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var o object
	if json.Unmarshal(b, &o) != nil || o == nil {
		return nil, errRefused
	}
	return o, nil
}

// create never applies: a rerun cannot adopt or mutate an existing object.
func (k client) create(o object) (object, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return nil, errRefused
	}
	out, err := k.call("", b, "create", "-f", "-", "-o", "json")
	if err != nil {
		return nil, errRefused
	}
	var created object
	if json.Unmarshal(out, &created) != nil || created == nil {
		return nil, errRefused
	}
	return created, nil
}
func (k client) list(ns, kind string) ([]object, error) {
	b, err := k.call(ns, nil, "get", kind, "-o", "json")
	if err != nil {
		return nil, errRefused
	}
	var list struct {
		Items []object `json:"items"`
	}
	if json.Unmarshal(b, &list) != nil || list.Items == nil {
		return nil, errRefused
	}
	return list.Items, nil
}
func (k client) pause() error {
	select {
	case <-k.ctx.Done():
		return errRefused
	case <-time.After(k.interval):
		return nil
	}
}

// inventorySQL counts rows through query_to_xml so a table the migrations have
// not created yet is simply absent, rather than a failed statement.
const inventorySQL = `BEGIN READ ONLY; SELECT json_build_object('databaseOwner',(SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()),'ledgerPresent',to_regclass('public.portal_schema_version') IS NOT NULL,'appliedMigrations',CASE WHEN to_regclass('public.portal_schema_version') IS NULL THEN 0 ELSE (xpath('/row/n/text()',query_to_xml('SELECT count(*) AS n FROM public.portal_schema_version',false,true,'')))[1]::text::bigint END,'highestVersion',CASE WHEN to_regclass('public.portal_schema_version') IS NULL THEN 0 ELSE (xpath('/row/n/text()',query_to_xml('SELECT coalesce(max(version),0) AS n FROM public.portal_schema_version',false,true,'')))[1]::text::bigint END,'tables',(SELECT coalesce(json_object_agg(c.relname,(xpath('/row/n/text()',query_to_xml(format('SELECT count(*) AS n FROM %I.%I',n.nspname,c.relname),false,true,'')))[1]::text::bigint ORDER BY c.relname),'{}'::json) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p'))); COMMIT;`

func (k client) inventory(ns, primary string) (inventory, error) {
	if !regexp.MustCompile(`^(business-site-portal-db|restore)-[1-9][0-9]*$`).MatchString(primary) {
		return inventory{}, errRefused
	}
	b, err := k.call(ns, nil, "exec", primary, "--container=postgres", "--", "psql", "--username=postgres", "--dbname="+databaseName, "--set=ON_ERROR_STOP=1", "--tuples-only", "--no-align", "--quiet", "--command="+inventorySQL)
	var result inventory
	if err != nil || json.Unmarshal(bytes.TrimSpace(b), &result) != nil {
		return inventory{}, errRefused
	}
	return result, nil
}

// backup takes the fresh online backup and waits up to twenty minutes for it.
func (k client) backup(run string, s source) (receipt, error) {
	created, err := k.create(backupRequest(run))
	if err != nil || !uuid.MatchString(str(created, "metadata", "uid")) {
		return receipt{}, errRefused
	}
	uid := str(created, "metadata", "uid")
	for attempt := 0; attempt < 240; attempt++ {
		b, err := k.get(sourceNamespace, "backups.postgresql.cnpg.io", backupPrefix+run)
		if err != nil || b == nil || str(b, "metadata", "uid") != uid {
			return receipt{}, errRefused
		}
		switch str(b, "status", "phase") {
		case "completed":
			return validateBackup(b, run, uid, s.UID)
		case "", "new", "pending", "started", "running", "finalizing":
		default:
			return receipt{}, errRefused
		}
		if err = k.pause(); err != nil {
			return receipt{}, err
		}
	}
	return receipt{}, errRefused
}
func (k client) source() (source, error) {
	c, err := k.get(sourceNamespace, "clusters.postgresql.cnpg.io", clusterName)
	if err != nil {
		return source{}, err
	}
	s, err := k.get(sourceNamespace, "objectstores.barmancloud.cnpg.io", storeName)
	if err != nil {
		return source{}, err
	}
	return validateSource(c, s)
}
func (k client) primary() (string, error) {
	c, err := k.get(sourceNamespace, "clusters.postgresql.cnpg.io", clusterName)
	if err != nil || c == nil {
		return "", errRefused
	}
	return str(c, "status", "currentPrimary"), nil
}

func owns(o object, run string) bool {
	return o != nil && str(o, "metadata", "name") == restorePrefix+run && str(o, "metadata", "labels", ownerKey) == run && uuid.MatchString(str(o, "metadata", "uid"))
}
func cleanup(k client, run, expectedUID string) error {
	ns := restorePrefix + run
	o, err := k.get("", "namespaces", ns)
	if err != nil {
		return err
	}
	if o != nil {
		if !owns(o, run) || (expectedUID != "" && str(o, "metadata", "uid") != expectedUID) {
			return errRefused
		}
		// Raw DELETE carries a UID precondition; a same-name replacement is safe.
		body, _ := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": str(o, "metadata", "uid")}, "propagationPolicy": "Foreground"})
		if _, err = k.call("", body, "delete", "--raw=/api/v1/namespaces/"+ns, "-f", "-"); err != nil {
			return errRefused
		}
		if _, err = k.call("", nil, "wait", "--for=delete", "namespace/"+ns, "--timeout=10m"); err != nil {
			return errRefused
		}
	}
	// Namespace deletion includes the policy, Secret copy, ObjectStore,
	// Cluster and claim. Wait for every backing volume to be reclaimed too.
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
					return errRefused
				}
			}
		}
		if !remaining {
			remainingNS, err := k.get("", "namespaces", ns)
			if err != nil || remainingNS != nil {
				return errRefused
			}
			return nil
		}
		if err = k.pause(); err != nil {
			return err
		}
	}
	return errRefused
}

// refusal names the phase a proof stopped in. It never carries a value read
// from the cluster, so it is safe to print in a public log.
type refusal struct{ phase string }

func (r refusal) Error() string { return "refused in phase " + r.phase }

func prove(k client, run, sha string) (result object, err error) {
	phase := "preflight"
	// Registered first, so it runs after cleanup and reports a cleanup failure.
	defer func() {
		if err != nil {
			result, err = nil, refusal{phase}
		}
	}()
	ns := restorePrefix + run
	existing, err := k.get("", "namespaces", ns)
	if err != nil || existing != nil {
		return nil, errRefused
	}
	state, err := k.source()
	if err != nil {
		return nil, err
	}
	phase = "backup"
	backup, err := k.backup(run, state)
	if err != nil {
		return nil, err
	}
	if stateAfterBackup, err := k.source(); err != nil || stateAfterBackup != state {
		return nil, errRefused
	}
	phase = "live-snapshot"
	primary, err := k.primary()
	if err != nil {
		return nil, err
	}
	before, err := k.inventory(sourceNamespace, primary)
	if err != nil {
		return nil, err
	}
	// Archive the current WAL segment so the restore replays recent commits.
	if _, err = k.call(sourceNamespace, nil, "exec", primary, "--container=postgres", "--", "psql", "--username=postgres", "--dbname=postgres", "--set=ON_ERROR_STOP=1", "--quiet", "--command=SELECT pg_switch_wal();"); err != nil {
		return nil, errRefused
	}
	phase = "namespace"
	items := resources(run, state, backup.BackupID)
	if _, err = k.create(items[0]); err != nil {
		return nil, errRefused
	}
	created, err := k.get("", "namespaces", ns)
	if err != nil || !owns(created, run) {
		return nil, errRefused
	}
	nsUID := str(created, "metadata", "uid")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		cleanupClient := k
		cleanupClient.ctx = ctx
		if cleanup(cleanupClient, run, nsUID) != nil {
			phase = "cleanup"
			result, err = nil, errRefused
		}
	}()
	if _, err = k.create(items[1]); err != nil {
		return nil, errRefused
	}
	phase = "credential"
	secrets, err := k.list(ns, "secrets")
	if err != nil || len(secrets) != 0 {
		return nil, errRefused
	}
	secret, err := k.get(sourceNamespace, "secrets", credential)
	if err != nil || secret == nil || str(secret, "metadata", "name") != credential {
		return nil, errRefused
	}
	data, _ := value(secret, "data").(map[string]any)
	if len(data) != 3 {
		return nil, errRefused
	}
	for _, key := range []string{"ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "REGION"} {
		if v, ok := data[key].(string); !ok || v == "" {
			return nil, errRefused
		}
	}
	// No value or Secret body reaches stdout, stderr or the proof receipt.
	if _, err = k.create(object{"apiVersion": "v1", "kind": "Secret", "metadata": metadata(credential, ns, run), "type": "Opaque", "data": data}); err != nil {
		return nil, errRefused
	}
	phase = "restore"
	for _, item := range items[2:] {
		if _, err = k.create(item); err != nil {
			return nil, errRefused
		}
	}
	if _, err = k.call(ns, nil, "wait", "--for=condition=Ready", "cluster.postgresql.cnpg.io/restore", "--timeout=30m"); err != nil {
		return nil, errRefused
	}
	restored, err := k.get(ns, "clusters.postgresql.cnpg.io", "restore")
	if err != nil || restored == nil || number(restored, "status", "readyInstances") != 1 || !ready(restored, "Ready") || value(restored, "spec", "plugins") != nil || str(restored, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID") != backup.BackupID {
		return nil, errRefused
	}
	phase = "isolation"
	// Resolve the live service first and prove the probe works locally. Only
	// PostgreSQL's no-response result satisfies the negative connectivity check.
	const isolationProbe = `set -eu; pg_isready -h 127.0.0.1 -p 5432 -t 3 >/dev/null; getent ahostsv4 business-site-portal-db-rw.business-site.svc.cluster.local >/dev/null; set +e; pg_isready -h business-site-portal-db-rw.business-site.svc.cluster.local -p 5432 -t 3 >/dev/null; rc=$?; test "$rc" -eq 2`
	if _, err = k.call(ns, nil, "exec", str(restored, "status", "currentPrimary"), "--container=postgres", "--", "sh", "-c", isolationProbe); err != nil {
		return nil, errRefused
	}
	phase = "aggregates"
	recovered, err := k.inventory(ns, str(restored, "status", "currentPrimary"))
	if err != nil {
		return nil, err
	}
	if stateAfter, err := k.source(); err != nil || stateAfter != state {
		return nil, errRefused
	}
	primary, err = k.primary()
	if err != nil {
		return nil, err
	}
	after, err := k.inventory(sourceNamespace, primary)
	if err != nil {
		return nil, err
	}
	empty, err := compare(before, recovered, after)
	if err != nil {
		return nil, err
	}
	result = object{"sourceSHA": sha, "backupTaken": true, "restoreCompleted": true, "isolationProven": true, "aggregatesMatched": true, "cleanupVerified": true, "productionClusterStable": true, "backupName": backup.Name, "backupID": backup.BackupID, "endWal": backup.EndWAL, "ledgerEmpty": empty, "recovered": recovered}
	if empty {
		result["note"] = "The migration ledger is empty: the portal migration Job has not run yet, so the proof compared the database owner and an empty table set."
	}
	return result, nil
}

func invocation(env func(string) string) (string, string, error) {
	run, sha := env("GITHUB_RUN_ID"), env("GITHUB_SHA")
	if env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || env("PORTAL_RESTORE_CONFIRM") != "verify-portal-db-restore" || !digits.MatchString(run) || !commit.MatchString(sha) {
		return "", "", errRefused
	}
	return run, sha, nil
}
func main() {
	stage := "invocation"
	err := func() error {
		run, sha, err := invocation(os.Getenv)
		if err != nil {
			return err
		}
		mode := "prove"
		if len(os.Args) == 2 && os.Args[1] == "--cleanup" {
			mode = "cleanup"
		} else if len(os.Args) != 1 {
			return errRefused
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		k := client{ctx: ctx, interval: 5 * time.Second, command: func(ctx context.Context, args []string, input []byte) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "kubectl", args...)
			cmd.Stdin = bytes.NewReader(input)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
			// stderr may contain source content, endpoint details or credentials.
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil || len(out) > 4*1024*1024 {
				return nil, errRefused
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
		result, err := prove(k, run, sha)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}()
	if err != nil {
		var r refusal
		if errors.As(err, &r) {
			stage += "/" + r.phase
		}
		fmt.Fprintln(os.Stderr, "Portal database restore refused (phase: "+strings.ReplaceAll(stage, "\n", "")+").")
		os.Exit(1)
	}
}
