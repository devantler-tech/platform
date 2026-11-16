package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func obj(t *testing.T, text string) object {
	t.Helper()
	var value object
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func fixture(t *testing.T) (object, object) {
	return obj(t, `{"metadata":{"name":"business-site-portal-db","namespace":"business-site","uid":"00000000-0000-0000-0000-000000000001","generation":2},"spec":{"instances":2,"imageName":"ghcr.io/cloudnative-pg/postgresql:18.7-system-trixie","storage":{"size":"10Gi","storageClass":"hcloud"},"affinity":{"nodeSelector":{"platform.devantler.tech/baseline-worker":"true"}},"bootstrap":{"initdb":{"database":"portal","owner":"portal_migrator"}},"plugins":[{"name":"barman-cloud.cloudnative-pg.io","enabled":true,"isWALArchiver":true,"parameters":{"barmanObjectName":"business-site-portal-db"}}]},"status":{"readyInstances":2,"currentPrimary":"business-site-portal-db-1","instanceNames":["business-site-portal-db-1","business-site-portal-db-2"],"conditions":[{"type":"Ready","status":"True"},{"type":"ContinuousArchiving","status":"True"}]}}`),
		obj(t, `{"metadata":{"name":"business-site-portal-db","namespace":"business-site","uid":"00000000-0000-0000-0000-000000000002","generation":1},"spec":{"retentionPolicy":"30d","configuration":{"destinationPath":"s3://platform-backups/cnpg/business-site-portal-db","endpointURL":"https://00000000000000000000000000000000.r2.cloudflarestorage.com","s3Credentials":{"accessKeyId":{"name":"business-site-portal-db-backup-r2","key":"ACCESS_KEY_ID"},"secretAccessKey":{"name":"business-site-portal-db-backup-r2","key":"SECRET_ACCESS_KEY"},"region":{"name":"business-site-portal-db-backup-r2","key":"REGION"}}}}}`)
}

func completedBackup(t *testing.T) object {
	return obj(t, `{"metadata":{"name":"business-site-portal-db-restore-proof-12345","namespace":"business-site","uid":"00000000-0000-0000-0000-000000000004","creationTimestamp":"2026-11-16T02:00:00Z","labels":{"platform.devantler.tech/restore-proof-run":"12345"}},"spec":{"cluster":{"name":"business-site-portal-db"},"method":"plugin","pluginConfiguration":{"name":"barman-cloud.cloudnative-pg.io"},"target":"primary"},"status":{"phase":"completed","method":"plugin","backupId":"20261116T020005","endWal":"000000010000000000000007","startedAt":"2026-11-16T02:00:05Z","stoppedAt":"2026-11-16T02:00:40Z","pluginMetadata":{"clusterUID":"00000000-0000-0000-0000-000000000001","name":"barman-cloud.cloudnative-pg.io"}}}`)
}

// A wrong archive, credential or placement would let a restore of some other
// database, or a refused Pod, pass for this one. Valid input must produce a
// separate, one-instance, non-archiving restore of the proven backup.
func TestSourceAndIsolatedRestorePlan(t *testing.T) {
	c, s := fixture(t)
	source, err := validateSource(c, s)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(resources("12345", source, "20261116T020005"))
	var plan []object
	_ = json.Unmarshal(encoded, &plan)
	if len(plan) != 4 {
		t.Fatalf("want namespace, isolation policy, store and cluster; got %d", len(plan))
	}
	if str(plan[0], "metadata", "name") != "portal-db-restore-12345" || str(plan[0], "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" {
		t.Fatal("restore must run in its own restricted namespace")
	}
	cluster := plan[3]
	if str(cluster, "metadata", "namespace") != "portal-db-restore-12345" || number(cluster, "spec", "instances") != 1 {
		t.Fatal("restore must be a separate one-instance database")
	}
	if value(cluster, "spec", "plugins") != nil || value(cluster, "spec", "backup") != nil {
		t.Fatal("restore must never archive into the live destination")
	}
	if str(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID") != "20261116T020005" {
		t.Fatal("restore must bind the proven fresh backup")
	}
	recoverySource := value(cluster, "spec", "externalClusters").([]any)[0].(map[string]any)
	if !reflect.DeepEqual(value(recoverySource, "plugin", "parameters"), map[string]any{"barmanObjectName": "portal-db-restore", "serverName": "business-site-portal-db"}) {
		t.Fatal("restore must read the live server's archive through its own store")
	}
	if str(cluster, "spec", "storage", "storageClass") != "hcloud" || str(cluster, "spec", "affinity", "nodeSelector", "platform.devantler.tech/baseline-worker") != "true" || str(cluster, "spec", "priorityClassName") != "hcloud-volume-workload" {
		t.Fatal("an hcloud restore claim must run on a baseline worker at the volume priority")
	}
	store := plan[2]
	if str(store, "spec", "configuration", "destinationPath") != "s3://platform-backups/cnpg/business-site-portal-db" || str(store, "spec", "configuration", "s3Credentials", "accessKeyId", "name") != "business-site-portal-db-backup-r2" {
		t.Fatal("restore must read the portal archive with the copied backup credential")
	}
	if value(store, "spec", "retentionPolicy") != nil {
		t.Fatal("a second retention policy would prune the live archive")
	}
	deny := value(plan[1], "spec", "egressDeny").([]any)[0].(map[string]any)
	selectors := deny["toEndpoints"].([]any)
	if selectors[0].(map[string]any)["matchLabels"].(map[string]any)["k8s:io.kubernetes.pod.namespace"] != "business-site" {
		t.Fatal("production database traffic must be denied")
	}
}

// A cold Cilium node must learn the backup endpoint through the DNS proxy.
// DNS interception is limited to kube-dns; HTTPS remains limited to R2.
func TestRestoreBackupDNSInterception(t *testing.T) {
	c, s := fixture(t)
	source, err := validateSource(c, s)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(resources("12345", source, "20261116T020005")[1])
	policy := obj(t, string(encoded))
	var dns, https object
	for _, entry := range value(policy, "spec", "egress").([]any) {
		rule := entry.(map[string]any)
		if value(rule, "toEndpoints") != nil {
			dns = rule
		}
		if value(rule, "toFQDNs") != nil {
			https = rule
		}
	}
	wantDNS := obj(t, `{"toEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}}],"toPorts":[{"ports":[{"port":"53","protocol":"UDP"},{"port":"53","protocol":"TCP"}],"rules":{"dns":[{"matchPattern":"*"}]}}]}`)
	if !reflect.DeepEqual(dns, wantDNS) {
		t.Fatal("restore DNS must intercept UDP and TCP only through kube-dns")
	}
	wantHTTPS := obj(t, `{"toFQDNs":[{"matchName":"00000000000000000000000000000000.r2.cloudflarestorage.com"}],"toPorts":[{"ports":[{"port":"443","protocol":"TCP"}]}]}`)
	if !reflect.DeepEqual(https, wantHTTPS) {
		t.Fatal("backup access must remain limited to the R2 endpoint over HTTPS")
	}
}

func TestRefuseUnsafeSources(t *testing.T) {
	for _, mutation := range []string{"other-store", "server-name", "other-key", "other-bucket", "unhealthy", "not-archiving", "archiver-off", "duplicate-plugin", "deleting", "endpoint-injection", "other-database", "other-storage", "unpinned"} {
		t.Run(mutation, func(t *testing.T) {
			c, s := fixture(t)
			spec := c["spec"].(map[string]any)
			entry := spec["plugins"].([]any)[0].(map[string]any)
			config := s["spec"].(map[string]any)["configuration"].(map[string]any)
			switch mutation {
			case "other-store":
				entry["parameters"].(map[string]any)["barmanObjectName"] = "customer-identity-db"
			case "server-name":
				entry["parameters"].(map[string]any)["serverName"] = "business-site-portal-db-old"
			case "other-key":
				config["s3Credentials"].(map[string]any)["accessKeyId"].(map[string]any)["name"] = "platform-backup-r2"
			case "other-bucket":
				config["destinationPath"] = "s3://platform-backups/cnpg/customer-identity-db"
			case "unhealthy":
				c["status"].(map[string]any)["readyInstances"] = float64(1)
			case "not-archiving":
				c["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
			case "archiver-off":
				entry["isWALArchiver"] = false
			case "duplicate-plugin":
				spec["plugins"] = append(spec["plugins"].([]any), entry)
			case "deleting":
				c["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-16T02:00:00Z"
			case "endpoint-injection":
				config["endpointURL"] = "https://attacker.example/"
			case "other-database":
				spec["bootstrap"].(map[string]any)["initdb"].(map[string]any)["owner"] = "app"
			case "other-storage":
				spec["storage"].(map[string]any)["storageClass"] = "longhorn"
			case "unpinned":
				delete(spec, "affinity")
			}
			if _, err := validateSource(c, s); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

// Only this run's own completed backup of the live cluster may be restored.
func TestBackupReceipt(t *testing.T) {
	got, err := validateBackup(completedBackup(t), "12345", "00000000-0000-0000-0000-000000000004", "00000000-0000-0000-0000-000000000001")
	if err != nil || got != (receipt{"business-site-portal-db-restore-proof-12345", "20261116T020005", "000000010000000000000007"}) {
		t.Fatalf("valid backup refused: %v %v", got, err)
	}
	for _, mutation := range []string{"foreign-cluster", "failed", "replaced", "other-run", "standby-target", "missing-wal", "stale", "unordered", "bad-id"} {
		t.Run(mutation, func(t *testing.T) {
			b := completedBackup(t)
			status := b["status"].(map[string]any)
			switch mutation {
			case "foreign-cluster":
				status["pluginMetadata"].(map[string]any)["clusterUID"] = "00000000-0000-0000-0000-000000000099"
			case "failed":
				status["phase"] = "failed"
			case "replaced":
				b["metadata"].(map[string]any)["uid"] = "00000000-0000-0000-0000-000000000098"
			case "other-run":
				b["metadata"].(map[string]any)["labels"].(map[string]any)["platform.devantler.tech/restore-proof-run"] = "99999"
			case "standby-target":
				b["spec"].(map[string]any)["target"] = "prefer-standby"
			case "missing-wal":
				delete(status, "endWal")
			case "stale":
				status["startedAt"] = "2026-11-16T01:59:59Z"
			case "unordered":
				status["stoppedAt"] = "2026-11-16T02:00:04Z"
			case "bad-id":
				status["backupId"] = "latest"
			}
			if _, err := validateBackup(b, "12345", "00000000-0000-0000-0000-000000000004", "00000000-0000-0000-0000-000000000001"); err == nil {
				t.Fatal("unproven backup accepted")
			}
		})
	}
}

func TestAggregateComparison(t *testing.T) {
	live := func() inventory {
		return inventory{Owner: "portal_migrator", Ledger: true, Applied: 24, Highest: 24, Tables: map[string]int64{"portal_schema_version": 24, "portal_sessions": 3, "portal_contacts": 5}}
	}
	before, after := live(), live()
	after.Tables["portal_sessions"] = 4
	empty, err := compare(before, live(), after)
	if err != nil || empty {
		t.Fatalf("matching aggregates refused: %v", err)
	}
	for _, mutation := range []string{"lost-row", "future-row", "lost-table", "extra-table", "ledger-behind", "ledger-gap", "ledger-rows", "other-owner", "no-tables", "ledger-vanished"} {
		t.Run(mutation, func(t *testing.T) {
			recovered := live()
			switch mutation {
			case "lost-row":
				recovered.Tables["portal_contacts"] = 4
			case "future-row":
				recovered.Tables["portal_sessions"] = 5
			case "lost-table":
				delete(recovered.Tables, "portal_contacts")
			case "extra-table":
				recovered.Tables["portal_orders"] = 0
			case "ledger-behind":
				recovered.Applied, recovered.Highest, recovered.Tables["portal_schema_version"] = 23, 23, 23
			case "ledger-gap":
				recovered.Highest = 25
			case "ledger-rows":
				recovered.Tables["portal_schema_version"] = 23
			case "other-owner":
				recovered.Owner = "app"
			case "no-tables":
				recovered.Tables = nil
			case "ledger-vanished":
				recovered = inventory{Owner: "portal_migrator", Tables: map[string]int64{"portal_sessions": 3, "portal_contacts": 5}}
			}
			if _, err := compare(before, recovered, after); err == nil {
				t.Fatal("mismatched aggregates accepted")
			}
		})
	}
}

// Before the migration Job has run, the ledger is empty. The proof still
// passes on matching aggregates and reports that only the database owner and
// an empty table set were compared; a ledger on only one side still fails.
func TestEmptyLedgerComparison(t *testing.T) {
	fresh := inventory{Owner: "portal_migrator", Tables: map[string]int64{}}
	empty, err := compare(fresh, fresh, fresh)
	if err != nil || !empty {
		t.Fatalf("empty matching ledger refused or unreported: %v", err)
	}
	migrated := inventory{Owner: "portal_migrator", Ledger: true, Applied: 1, Highest: 1, Tables: map[string]int64{"portal_schema_version": 1}}
	if _, err := compare(fresh, fresh, migrated); err == nil {
		t.Fatal("a ledger written during the proof was accepted")
	}
	if _, err := compare(fresh, inventory{Owner: "app", Tables: map[string]int64{}}, fresh); err == nil {
		t.Fatal("an empty database that is not the portal's was accepted")
	}
	if _, err := compare(fresh, inventory{Owner: "portal_migrator"}, fresh); err == nil {
		t.Fatal("a snapshot without a table listing was accepted")
	}
	// The migrations create the ledger with their tables, so tables without a
	// ledger, or beside an empty one, are not the unmigrated portal schema.
	ledgerless := inventory{Owner: "portal_migrator", Tables: map[string]int64{"portal_sessions": 3}}
	if _, err := compare(ledgerless, ledgerless, ledgerless); err == nil {
		t.Fatal("tables without a ledger passed as an empty table set")
	}
	emptyLedger := inventory{Owner: "portal_migrator", Ledger: true, Tables: map[string]int64{"portal_schema_version": 0}}
	if empty, err := compare(emptyLedger, emptyLedger, emptyLedger); err != nil || !empty {
		t.Fatalf("an empty ledger standing alone was refused or unreported: %v", err)
	}
	emptyLedger.Tables["portal_sessions"] = 3
	if _, err := compare(emptyLedger, emptyLedger, emptyLedger); err == nil {
		t.Fatal("tables beside an empty ledger passed as an empty table set")
	}
}

// The positive control must come from a live standby when there is one, and
// a malformed instance name must never reach kubectl exec.
func TestPositiveControlInstance(t *testing.T) {
	for _, tc := range []struct {
		names, primary, peer string
		ok                   bool
	}{
		{`["business-site-portal-db-1","business-site-portal-db-2"]`, "business-site-portal-db-1", "business-site-portal-db-2", true},
		{`["business-site-portal-db-2","business-site-portal-db-3"]`, "business-site-portal-db-3", "business-site-portal-db-2", true},
		{`["business-site-portal-db-1"]`, "business-site-portal-db-1", "business-site-portal-db-1", true},
		{`[]`, "business-site-portal-db-1", "", false},
		{`["business-site-portal-db-1","x; sh"]`, "business-site-portal-db-1", "", false},
	} {
		c, _ := fixture(t)
		status := c["status"].(map[string]any)
		status["currentPrimary"] = tc.primary
		var names []any
		_ = json.Unmarshal([]byte(tc.names), &names)
		status["instanceNames"] = names
		k := client{ctx: context.Background(), command: func(context.Context, []string, []byte) ([]byte, error) {
			return json.Marshal(c)
		}}
		primary, peer, err := k.instances()
		if (err == nil) != tc.ok || (tc.ok && (primary != tc.primary || peer != tc.peer)) {
			t.Fatalf("%s: got %s %s %v", tc.names, primary, peer, err)
		}
	}
}

func TestDispatchBoundary(t *testing.T) {
	base := map[string]string{"GITHUB_RUN_ID": "12345", "GITHUB_SHA": strings.Repeat("a", 40), "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "PORTAL_RESTORE_CONFIRM": "verify-portal-db-restore"}
	if _, _, err := invocation(func(key string) string { return base[key] }); err != nil {
		t.Fatal(err)
	}
	for key := range base {
		t.Run(key, func(t *testing.T) {
			if _, _, err := invocation(func(k string) string {
				if k == key {
					return "wrong"
				}
				return base[k]
			}); err == nil {
				t.Fatal("unauthorized dispatch accepted")
			}
		})
	}
}

// Run the orchestration against an API boundary double. Only the kubectl
// transport is substituted; validation, comparison, ownership and cleanup use
// the production code. Any production write other than this run's Backup, any
// unproven backup reaching a restore, or a lost cleanup fails this test.
func TestProofCleanupAcrossFailures(t *testing.T) {
	phases := map[string]string{"": "", "empty-ledger": "", "backup-failed": "backup", "foreign-backup": "backup", "source-drift": "backup", "policy": "namespace", "foreign-secret": "credential", "secret": "credential", "store": "restore", "cluster": "restore", "wait": "restore", "rebound": "restore", "archive-stalled": "live-snapshot", "production-down": "isolation", "egress": "isolation", "inventory": "aggregates", "data": "aggregates", "cleanup": "cleanup"}
	for failure, phase := range phases {
		t.Run(failure, func(t *testing.T) {
			c, s := fixture(t)
			objects := map[string]object{"business-site/clusters.postgresql.cnpg.io/business-site-portal-db": c, "business-site/objectstores.barmancloud.cnpg.io/business-site-portal-db": s, "business-site/secrets/" + credential: obj(t, `{"metadata":{"name":"business-site-portal-db-backup-r2"},"data":{"ACCESS_KEY_ID":"a2V5","SECRET_ACCESS_KEY":"c2VjcmV0","REGION":"YXV0bw=="}}`)}
			created, deleted, polled, archiverReads := false, false, 0, 0
			live := inventory{Owner: "portal_migrator", Ledger: true, Applied: 24, Highest: 24, Tables: map[string]int64{"portal_schema_version": 24, "portal_sessions": 3}}
			if failure == "empty-ledger" {
				live = inventory{Owner: "portal_migrator", Tables: map[string]int64{}}
			}
			k := client{ctx: context.Background(), command: func(_ context.Context, args []string, input []byte) ([]byte, error) {
				if len(args) < 5 || strings.Join(args[:3], " ") != "--context admin@prod --request-timeout=30s" {
					t.Fatal("wrong cluster")
				}
				args = args[3:]
				ns := ""
				if args[0] == "--namespace" {
					ns = args[1]
					args = args[2:]
				}
				encode := func(v any) ([]byte, error) { return json.Marshal(v) }
				switch args[0] {
				case "get":
					if args[1] == "secrets" && len(args) == 4 {
						items := []object{}
						if failure == "foreign-secret" {
							items = append(items, obj(t, `{"metadata":{"name":"platform-backup-r2"}}`))
						}
						return encode(object{"items": items})
					}
					if args[1] == "persistentvolumes" {
						return encode(object{"items": []object{}})
					}
					key := ns + "/" + args[1] + "/" + args[2]
					if args[1] == "backups.postgresql.cnpg.io" && objects[key] != nil {
						// The first read sees the backup running; the next sees it finished.
						polled++
						if polled == 2 {
							finished := completedBackup(t)
							switch failure {
							case "backup-failed":
								finished["status"].(map[string]any)["phase"] = "failed"
							case "foreign-backup":
								finished["status"].(map[string]any)["pluginMetadata"].(map[string]any)["clusterUID"] = "00000000-0000-0000-0000-000000000099"
							case "source-drift":
								c["metadata"].(map[string]any)["generation"] = float64(3)
							}
							objects[key] = finished
						}
					}
					o := objects[key]
					if o == nil {
						return nil, nil
					}
					return encode(o)
				case "create":
					var o object
					if json.Unmarshal(input, &o) != nil {
						t.Fatal("invalid request")
					}
					n, kind := str(o, "metadata", "namespace"), str(o, "kind")
					if n == "business-site" {
						if kind != "Backup" || !reflect.DeepEqual(o, backupRequest("12345")) {
							t.Fatal("restore mutated the production namespace")
						}
						o["metadata"].(map[string]any)["uid"] = "00000000-0000-0000-0000-000000000004"
						o["status"] = object{"phase": "running"}
						objects["business-site/backups.postgresql.cnpg.io/business-site-portal-db-restore-proof-12345"] = o
						return encode(o)
					}
					if kind != "Namespace" && n != "portal-db-restore-12345" {
						t.Fatal("restore object outside its namespace")
					}
					if (failure == "policy" && kind == "CiliumNetworkPolicy") || (failure == "secret" && kind == "Secret") || (failure == "store" && kind == "ObjectStore") || (failure == "cluster" && kind == "Cluster") {
						return nil, errors.New("transport failed")
					}
					o["metadata"].(map[string]any)["uid"] = "00000000-0000-0000-0000-000000000003"
					if kind == "Namespace" {
						created = true
						objects["/namespaces/portal-db-restore-12345"] = o
					}
					if kind == "Cluster" {
						o["status"] = object{"readyInstances": float64(1), "currentPrimary": "restore-1", "conditions": []any{object{"type": "Ready", "status": "True"}}}
						if failure == "rebound" {
							o["spec"].(map[string]any)["bootstrap"].(map[string]any)["recovery"].(map[string]any)["recoveryTarget"] = object{"backupID": "20261115T173017"}
						}
						objects[n+"/clusters.postgresql.cnpg.io/restore"] = o
					}
					return encode(o)
				case "exec":
					command := strings.Join(args, " ")
					if strings.Contains(command, "pg_isready") {
						if ns == "business-site" {
							// The positive control runs from the standby, through the live service.
							if args[1] != "business-site-portal-db-2" || !strings.Contains(command, "--host="+liveService) {
								t.Fatal("positive control did not use the standby and the live service")
							}
							if failure == "production-down" {
								return nil, errors.New("no response")
							}
							return nil, nil
						}
						if ns != "portal-db-restore-12345" {
							t.Fatal("isolation probe ran outside the restore")
						}
						if failure == "egress" {
							return nil, errors.New("production reachable")
						}
						return nil, nil
					}
					if strings.Contains(command, "pg_switch_wal") {
						if ns != "business-site" || args[1] != "business-site-portal-db-1" {
							t.Fatal("WAL switch must run on the live primary")
						}
						return []byte("000000010000000000000008\n"), nil
					}
					if strings.Contains(command, "pg_stat_archiver") {
						// The archiver reports the switched segment on its second read.
						archiverReads++
						if archiverReads < 2 || failure == "archive-stalled" {
							return []byte("000000010000000000000007\n"), nil
						}
						return []byte("000000010000000000000008\n"), nil
					}
					if !strings.Contains(command, "--dbname=portal") || !strings.Contains(command, "--command=BEGIN READ ONLY;") {
						t.Fatal("aggregate query must be read-only on the portal database")
					}
					if ns == "business-site" {
						return encode(live)
					}
					if failure == "inventory" {
						return nil, errors.New("query failed")
					}
					recovered := live
					if failure == "data" {
						recovered.Tables = map[string]int64{"portal_schema_version": 24, "portal_sessions": 2}
					}
					return encode(recovered)
				case "wait":
					if failure == "wait" && ns != "" {
						return nil, errors.New("not ready")
					}
					return nil, nil
				case "delete":
					if args[1] != "--raw=/api/v1/namespaces/portal-db-restore-12345" {
						t.Fatal("wrong delete target")
					}
					var options object
					_ = json.Unmarshal(input, &options)
					if str(options, "preconditions", "uid") != "00000000-0000-0000-0000-000000000003" {
						t.Fatal("deletion lacks UID precondition")
					}
					if failure == "cleanup" {
						return nil, errors.New("delete failed")
					}
					deleted = true
					delete(objects, "/namespaces/portal-db-restore-12345")
					return nil, nil
				default:
					t.Fatalf("unexpected API operation: %v", args)
				}
				return nil, errors.New("unexpected operation")
			}}
			proof, err := prove(k, "12345", strings.Repeat("a", 40))
			if phase == "" {
				if err != nil {
					t.Fatalf("proof failed: %v", err)
				}
				for _, key := range []string{"backupTaken", "restoreCompleted", "isolationProven", "aggregatesMatched", "cleanupVerified"} {
					if proof[key] != true {
						t.Fatalf("receipt lacks %s", key)
					}
				}
				if proof["sourceSHA"] != strings.Repeat("a", 40) || proof["backupID"] != "20261116T020005" || proof["ledgerEmpty"] != (failure == "empty-ledger") || (proof["note"] != nil) != (failure == "empty-ledger") {
					t.Fatalf("receipt not bound to this run: %v", proof)
				}
				encoded, _ := json.Marshal(proof)
				if strings.Contains(string(encoded), "c2VjcmV0") || strings.Contains(string(encoded), "a2V5") {
					t.Fatal("receipt leaked the backup credential")
				}
			} else {
				var r refusal
				if err == nil || proof != nil || !errors.As(err, &r) || r.phase != phase {
					t.Fatalf("failed proof misreported: %v", err)
				}
			}
			switch phase {
			case "backup", "live-snapshot":
				if created {
					t.Fatal("an unproven backup or unarchived snapshot reached a restore")
				}
			case "cleanup":
			default:
				if !created || !deleted {
					t.Fatal("temporary namespace leaked or was never created")
				}
			}
		})
	}
}

func TestCleanupRefusesForeignNamespace(t *testing.T) {
	foreign := obj(t, `{"metadata":{"name":"portal-db-restore-12345","uid":"00000000-0000-0000-0000-000000000001","labels":{"platform.devantler.tech/restore-proof-run":"99999"}}}`)
	k := client{ctx: context.Background(), command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[3] != "get" {
			t.Fatal("foreign namespace was mutated")
		}
		return json.Marshal(foreign)
	}}
	if cleanup(k, "12345", "") == nil {
		t.Fatal("foreign cleanup accepted")
	}
}

// Characterize kubectl's raw DELETE transport against a real HTTP server: a
// UID in a manifest is ignored by normal delete, so exercise the actual command.
func TestRawNamespaceDeleteCarriesUID(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not installed")
	}
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/api/v1/namespaces/portal-db-restore-12345" {
			t.Error("wrong raw request")
		}
		var options object
		_ = json.NewDecoder(r.Body).Decode(&options)
		if str(options, "preconditions", "uid") != "00000000-0000-0000-0000-000000000003" {
			t.Error("UID precondition missing")
		}
		seen = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`))
	}))
	defer server.Close()
	config := filepath.Join(t.TempDir(), "config")
	content := "apiVersion: v1\nkind: Config\nclusters:\n- name: fake\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fake\n    user: fake\nusers:\n- name: fake\n  user: {}\n"
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("kubectl", "--kubeconfig", config, "--context", "admin@prod", "delete", "--raw=/api/v1/namespaces/portal-db-restore-12345", "-f", "-")
	cmd.Stdin = strings.NewReader(`{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"00000000-0000-0000-0000-000000000003"}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl raw DELETE failed: %s", out)
	}
	if !seen {
		t.Fatal("raw request did not reach server")
	}
}
