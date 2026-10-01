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

func fixture(t *testing.T) (object, object, object) {
	return obj(t, `{"metadata":{"name":"wedding-db","namespace":"wedding-app","uid":"00000000-0000-0000-0000-000000000001","generation":8},"spec":{"instances":3,"imageName":"ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie","storage":{"size":"2Gi","storageClass":"longhorn-wffc"},"plugins":[{"name":"barman-cloud.cloudnative-pg.io","enabled":true,"isWALArchiver":true,"parameters":{"barmanObjectName":"wedding-db-dedicated","serverName":"wedding-db-20260909"}}]},"status":{"readyInstances":3,"currentPrimary":"wedding-db-1","conditions":[{"type":"Ready","status":"True"},{"type":"ContinuousArchiving","status":"True"}]}}`),
		obj(t, `{"metadata":{"name":"wedding-db-dedicated","namespace":"wedding-app","uid":"00000000-0000-0000-0000-000000000002","generation":1},"spec":{"configuration":{"destinationPath":"s3://wedding-db-backups/cnpg/wedding-db","endpointURL":"https://00000000000000000000000000000000.r2.cloudflarestorage.com","s3Credentials":{"accessKeyId":{"name":"wedding-db-backup-r2-dedicated","key":"ACCESS_KEY_ID"},"secretAccessKey":{"name":"wedding-db-backup-r2-dedicated","key":"SECRET_ACCESS_KEY"},"region":{"name":"wedding-db-backup-r2-dedicated","key":"REGION"}}}}}`),
		obj(t, `{"metadata":{"name":"wedding-db-dedicated-proof-12345","namespace":"wedding-app"},"spec":{"cluster":{"name":"wedding-db"},"method":"plugin","pluginConfiguration":{"name":"barman-cloud.cloudnative-pg.io"}},"status":{"phase":"completed","backupId":"20261001T074121","pluginMetadata":{"clusterUID":"00000000-0000-0000-0000-000000000001","name":"barman-cloud.cloudnative-pg.io"}}}`)
}

// A wrong archive reference or credential would permit a shared-store restore
// to be reported as dedicated-only. Valid input must construct an isolated plan.
func TestDedicatedSourceAndIsolation(t *testing.T) {
	c, s, b := fixture(t)
	source, err := validateSource(c, s, b)
	if err != nil {
		t.Fatal(err)
	}
	items := resources("12345", source)
	encoded, _ := json.Marshal(items)
	var plan []object
	_ = json.Unmarshal(encoded, &plan)
	if len(plan) != 4 {
		t.Fatalf("want namespace, deny policy, store and cluster; got %d", len(plan))
	}
	cluster := plan[3]
	if str(cluster, "metadata", "namespace") != "wedding-restore-12345" || number(cluster, "spec", "instances") != 1 {
		t.Fatal("restore must be a separate one-instance database")
	}
	if value(cluster, "spec", "plugins") != nil {
		t.Fatal("restore must never archive into the live destination")
	}
	if str(cluster, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID") != "20261001T074121" {
		t.Fatal("restore must bind the proven fresh backup")
	}
	if str(plan[2], "spec", "configuration", "s3Credentials", "accessKeyId", "name") != "wedding-db-backup-r2-dedicated" {
		t.Fatal("restore must use only dedicated credential")
	}
	policy := plan[1]
	deny := value(policy, "spec", "egressDeny").([]any)[0].(map[string]any)
	selectors := deny["toEndpoints"].([]any)
	if selectors[0].(map[string]any)["matchLabels"].(map[string]any)["k8s:io.kubernetes.pod.namespace"] != "wedding-app" {
		t.Fatal("production database traffic must be denied")
	}
}

func TestRefuseUnsafeSources(t *testing.T) {
	for _, mutation := range []string{"shared-reference", "shared-key", "shared-bucket", "unhealthy", "wrong-server", "wrong-backup", "duplicate-plugin", "deleting", "endpoint-injection"} {
		t.Run(mutation, func(t *testing.T) {
			c, s, b := fixture(t)
			switch mutation {
			case "shared-reference":
				c["spec"].(map[string]any)["plugins"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["barmanObjectName"] = "wedding-db"
			case "shared-key":
				s["spec"].(map[string]any)["configuration"].(map[string]any)["s3Credentials"].(map[string]any)["accessKeyId"].(map[string]any)["name"] = "wedding-db-backup-r2"
			case "shared-bucket":
				s["spec"].(map[string]any)["configuration"].(map[string]any)["destinationPath"] = "s3://platform-backups/cnpg/wedding-db"
			case "unhealthy":
				c["status"].(map[string]any)["readyInstances"] = float64(2)
			case "wrong-server":
				c["spec"].(map[string]any)["plugins"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["serverName"] = "wedding-db"
			case "wrong-backup":
				b["status"].(map[string]any)["pluginMetadata"].(map[string]any)["clusterUID"] = "00000000-0000-0000-0000-000000000099"
			case "duplicate-plugin":
				p := c["spec"].(map[string]any)["plugins"].([]any)
				c["spec"].(map[string]any)["plugins"] = append(p, p[0])
			case "deleting":
				c["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-01T08:00:00Z"
			case "endpoint-injection":
				s["spec"].(map[string]any)["configuration"].(map[string]any)["endpointURL"] = "https://attacker.example/"
			}
			if _, err := validateSource(c, s, b); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

func TestCoreDataComparison(t *testing.T) {
	before := inventory{Pairs: 40, Guests: 80, Answers: 65, Bookings: 20, Latest: "2026-10-01T08:00:00Z"}
	after := before
	after.Answers = 66
	after.Latest = "2026-10-01T08:01:00Z"
	if err := compare(before, before, after); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing-guest", "missing-answer", "missing-booking", "old-data", "future-data", "missing-time", "empty"} {
		t.Run(mutation, func(t *testing.T) {
			recovered := before
			switch mutation {
			case "missing-guest":
				recovered.Guests--
			case "missing-answer":
				recovered.Answers--
			case "missing-booking":
				recovered.Bookings--
			case "old-data":
				recovered.Latest = "2026-10-01T07:54:59Z"
			case "future-data":
				recovered.Latest = "2026-10-01T08:01:01Z"
			case "missing-time":
				recovered.Latest = ""
			case "empty":
				recovered = inventory{}
			}
			if err := compare(before, recovered, after); err == nil {
				t.Fatal("incomplete recovered data accepted")
			}
		})
	}
}

func TestDispatchBoundary(t *testing.T) {
	base := map[string]string{"GITHUB_RUN_ID": "12345", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "WEDDING_RESTORE_CONFIRM": "verify-wedding-dedicated-restore"}
	if _, err := invocation(func(key string) string { return base[key] }); err != nil {
		t.Fatal(err)
	}
	for key := range base {
		t.Run(key, func(t *testing.T) {
			if _, err := invocation(func(k string) string {
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

// Run the orchestration against an API boundary double. Only external kubectl
// transport is substituted; plan validation, comparison, ownership and cleanup
// use the production code. Losing cleanup or switching any production write
// into this path must fail this test.
func TestProofCleanupAcrossFailures(t *testing.T) {
	for _, failure := range []string{"", "policy", "secret", "store", "cluster", "wait", "inventory", "data", "foreign-secret", "egress", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			c, s, b := fixture(t)
			objects := map[string]object{"wedding-app/clusters.postgresql.cnpg.io/wedding-db": c, "wedding-app/objectstores.barmancloud.cnpg.io/wedding-db-dedicated": s, "wedding-app/backups.postgresql.cnpg.io/wedding-db-dedicated-proof-12345": b, "wedding-app/secrets/" + credential: obj(t, `{"metadata":{"name":"wedding-db-backup-r2-dedicated"},"data":{"ACCESS_KEY_ID":"a2V5","SECRET_ACCESS_KEY":"c2VjcmV0","REGION":"YXV0bw=="}}`)}
			created, deleted := false, false
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
							items = append(items, obj(t, `{"metadata":{"name":"wedding-db-backup-r2"}}`))
						}
						return encode(object{"items": items})
					}
					if args[1] == "persistentvolumes" {
						return encode(object{"items": []object{}})
					}
					key := ns + "/" + args[1] + "/" + args[2]
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
					n := str(o, "metadata", "namespace")
					kind := str(o, "kind")
					if n == "wedding-app" {
						t.Fatal("restore mutated production namespace")
					}
					if (failure == "policy" && kind == "CiliumNetworkPolicy") || (failure == "secret" && kind == "Secret") || (failure == "store" && kind == "ObjectStore") || (failure == "cluster" && kind == "Cluster") {
						return nil, errors.New("transport failed")
					}
					meta := o["metadata"].(map[string]any)
					meta["uid"] = "00000000-0000-0000-0000-000000000003"
					if kind == "Namespace" {
						created = true
						objects["/namespaces/wedding-restore-12345"] = o
					}
					if kind == "Cluster" {
						o["status"] = object{"readyInstances": float64(1), "currentPrimary": "restore-1", "conditions": []any{object{"type": "Ready", "status": "True"}}}
						objects[n+"/clusters.postgresql.cnpg.io/restore"] = o
					}
					return encode(o)
				case "exec":
					if strings.Contains(strings.Join(args, " "), "pg_isready") {
						if failure == "egress" {
							return nil, errors.New("production reachable")
						}
						return nil, nil
					}
					if strings.Contains(strings.Join(args, " "), "pg_switch_wal") {
						return []byte("flushed"), nil
					}
					if failure == "inventory" && ns != "wedding-app" {
						return nil, errors.New("query failed")
					}
					inv := inventory{40, 80, 65, 20, "2026-10-01T08:00:00Z"}
					if failure == "data" && ns != "wedding-app" {
						inv.Guests--
					}
					return encode(inv)
				case "wait":
					if failure == "wait" && ns != "" {
						return nil, errors.New("not ready")
					}
					return nil, nil
				case "delete":
					if args[1] != "--raw=/api/v1/namespaces/wedding-restore-12345" {
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
					delete(objects, "/namespaces/wedding-restore-12345")
					return nil, nil
				default:
					t.Fatalf("unexpected API operation: %v", args)
				}
				return nil, errors.New("unexpected operation")
			}}
			proof, err := prove(k, "12345")
			if !created {
				t.Fatal("restore namespace was never created")
			}
			if failure == "" {
				if err != nil || proof["cleanupVerified"] != true {
					t.Fatalf("proof failed: %v", err)
				}
			} else if err == nil || proof != nil {
				t.Fatal("failed proof reported success")
			}
			if failure != "cleanup" && !deleted {
				t.Fatal("temporary namespace leaked after failure")
			}
		})
	}
}

func TestCleanupRefusesForeignNamespace(t *testing.T) {
	foreign := obj(t, `{"metadata":{"name":"wedding-restore-12345","uid":"00000000-0000-0000-0000-000000000001","labels":{"platform.devantler.tech/restore-proof-run":"99999"}}}`)
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
		if r.Method != "DELETE" || r.URL.Path != "/api/v1/namespaces/wedding-restore-12345" {
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
	content := fmtConfig(server.URL)
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("kubectl", "--kubeconfig", config, "--context", "admin@prod", "delete", "--raw=/api/v1/namespaces/wedding-restore-12345", "-f", "-")
	cmd.Stdin = strings.NewReader(`{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"00000000-0000-0000-0000-000000000003"}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl raw DELETE failed: %s", out)
	}
	if !seen {
		t.Fatal("raw request did not reach server")
	}
}
func fmtConfig(server string) string {
	return "apiVersion: v1\nkind: Config\nclusters:\n- name: fake\n  cluster:\n    server: " + server + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fake\n    user: fake\nusers:\n- name: fake\n  user: {}\n"
}
