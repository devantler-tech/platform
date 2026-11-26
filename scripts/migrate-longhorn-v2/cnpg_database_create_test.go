package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// databaseCreationFixture composes already-proven stages through native-shaped API boundaries.
func databaseCreationFixture(t *testing.T, scenario string) (*cnpgProofRun, string, string, *int) {
	t.Helper()
	storageAPI, activation := recoveryStorageAPI(t)
	_, activationRecord, err := activation.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hostname := activationRecord.Targets[temporaryV2Selections[2]].Name
	var host object
	for _, node := range storageAPI.s.nodes {
		if text(node, "metadata", "name") == hostname {
			host = node
			break
		}
	}
	if host == nil {
		t.Fatal("activation fixture has no recovered worker")
	}
	host["apiVersion"], host["kind"] = "v1", "Node"
	valueObject(host, "status", "nodeInfo")["operatingSystem"] = "linux"
	valueObject(host, "metadata")["resourceVersion"] = "10"
	valueObject(host, "metadata", "labels")["kubernetes.io/hostname"] = hostname
	valueObject(host, "metadata", "annotations")[scaleDownDisabled] = "true"
	allocatable := valueObject(host, "status", "allocatable")
	allocatable["memory"], allocatable["pods"] = "16Gi", "110"
	p, _ := recoveryIsolationOnHostFixture(t, "", host)
	if err := p.prepareIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	ns := p.record.Resources["Namespace"].Name
	secret := object{"apiVersion": "v1", "kind": "Secret", "metadata": object{"name": "archive-read", "namespace": ns, "uid": "77777777-7777-4777-8777-777777777777", "resourceVersion": "10", "labels": object{"storage.platform.devantler.tech/recovery-run": "1234", "app.kubernetes.io/managed-by": "github-actions"}}, "type": "Opaque", "immutable": true, "data": object{"ACCESS_KEY_ID": base64.StdEncoding.EncodeToString([]byte("synthetic-access")), "SECRET_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte("synthetic-secret")), "REGION": base64.StdEncoding.EncodeToString([]byte("auto"))}}
	fingerprint, err := recoveryCredentialData(secret)
	if err != nil {
		t.Fatal(err)
	}
	p.record.Credential = &cnpgCredentialBinding{Source: benchmarkResource{Name: p.record.Plan.Source.Credential, Namespace: "umami", UID: "66666666-6666-4666-8666-666666666666"}, DataSHA: fingerprint}
	p.record.Resources["Secret"], p.record.Phase = benchmarkIdentity(secret), "credential-created"
	sourceSecret, err := cloneRecoverySpec(secret)
	if err != nil {
		t.Fatal(err)
	}
	sourceSecret["metadata"].(object)["name"] = p.record.Plan.Source.Credential
	sourceSecret["metadata"].(object)["namespace"] = "umami"
	sourceSecret["metadata"].(object)["uid"] = "66666666-6666-4666-8666-666666666666"
	if scenario == "source key changed" {
		sourceSecret["data"].(object)["SECRET_ACCESS_KEY"] = base64.StdEncoding.EncodeToString([]byte("rotated-synthetic-key"))
	}
	cm := ownedCNPGJournalFixture(t, p.record)
	cm["metadata"].(object)["uid"] = p.journalUID
	delegate := p.c.run
	original, _, backup := recoveryPlanningFixture(t)
	value(*backup, "spec").(object)["target"] = "primary"
	cc, cnpgPin, _ := operatorEnvironmentFixture(t, "")
	bc, barmanPin := barmanEnvironmentFixture(t, "")
	_, admitted := recoveryAdmissionFixture(t)
	admitted["spec"].(object)["imageName"] = "ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie@sha256:" + strings.Repeat("a", 64)
	value(admitted, "spec", "affinity").(object)["nodeSelector"] = object{"kubernetes.io/hostname": hostname}
	value(admitted, "spec", "monitoring").(object)["customQueriesConfigMap"] = []any{object{"name": "cnpg-default-monitoring", "key": "queries"}}
	created := map[string]object{}
	creates := 0
	namespaceReplacedDuringStorage := false
	p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			var persisted cnpgProofRecord
			if json.Unmarshal([]byte(text(cm, "data", "record")), &persisted) != nil {
				t.Fatal("invalid fixture journal")
			}
			if scenario == "main advanced after intent" && persisted.Phase == "objectstore-requested" {
				return []byte(strings.Repeat("b", 40) + "\trefs/heads/main\n"), nil
			}
			return delegate(ctx, input, program, args...)
		}
		if program != "kubectl" || len(args) < 4 {
			return nil, errors.New("unexpected database creation command")
		}
		if args[2] == "patch" {
			var ops []object
			if json.Unmarshal(input, &ops) != nil || len(ops) != 3 || ops[0]["value"] != p.journalUID || ops[1]["value"] != text(cm, "metadata", "resourceVersion") {
				t.Fatal("journal update lacks UID/RV fence")
			}
			cm["data"].(object)["record"] = text(ops[2], "value")
			cm["metadata"].(object)["resourceVersion"] = "11"
			return []byte("patched"), nil
		}
		joined := strings.Join(args, " ")
		if args[2] == "logs" {
			return cc.run(ctx, input, program, args...)
		}
		if args[2] == "get" {
			kind := args[3]
			if kind == "node" && args[4] == hostname {
				if scenario == "worker lost capacity after intent" && p.record.Phase == "objectstore-requested" {
					changed, err := cloneRecoverySpec(host)
					if err != nil {
						t.Fatal(err)
					}
					valueObject(changed, "status", "allocatable")["memory"] = "512Mi"
					return json.Marshal(changed)
				}
				return json.Marshal(host)
			}
			if kind == "pods" && args[len(args)-1] == "--all-namespaces" {
				return json.Marshal(object{"items": storageAPI.s.pods})
			}
			if kind == "namespace" && (scenario == "namespace replaced after intent" || namespaceReplacedDuringStorage) {
				var persisted cnpgProofRecord
				if json.Unmarshal([]byte(text(cm, "data", "record")), &persisted) != nil {
					t.Fatal("invalid fixture journal")
				}
				if persisted.Phase == "objectstore-requested" {
					raw, e := delegate(ctx, input, program, args...)
					if e != nil {
						return nil, e
					}
					var changed object
					if json.Unmarshal(raw, &changed) != nil {
						t.Fatal("invalid namespace fixture")
					}
					changed["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
					return json.Marshal(changed)
				}
			}
			if kind == "customresourcedefinition" {
				if args[4] == "objectstores.barmancloud.cnpg.io" {
					return bc.run(ctx, input, program, args...)
				}
				return cc.run(ctx, input, program, args...)
			}
			if kind == "nodes" || strings.Contains(joined, "--namespace=cnpg-system") {
				if kind == "deployment" && args[4] == "plugin-barman-cloud" {
					return bc.run(ctx, input, program, args...)
				}
				if kind == "pods" || kind == "replicasets" || kind == "nodes" {
					left, e := cc.run(ctx, input, program, args...)
					if e != nil {
						return nil, e
					}
					right, e := bc.run(ctx, input, program, args...)
					if e != nil {
						return nil, e
					}
					if kind == "nodes" {
						right, e = delegate(ctx, input, program, args...)
						if e != nil {
							return nil, e
						}
					}
					var a, b object
					if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
						t.Fatal("bad inventory fixture")
					}
					a["items"] = append(value(a, "items").([]any), value(b, "items").([]any)...)
					return json.Marshal(a)
				}
				return cc.run(ctx, input, program, args...)
			}
			if kind == "configmap" {
				return json.Marshal(cm)
			}
			if kind == "secret" {
				if args[4] == p.record.Plan.Source.Credential {
					return json.Marshal(sourceSecret)
				}
				if scenario == "foreign secret" {
					secret["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
				}
				return json.Marshal(secret)
			}
			if strings.Contains(joined, "--namespace="+ns) && args[4] == "--output=json" {
				items := []any{}
				if kind == "secrets" {
					if scenario == "foreign secret" {
						secret["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
					}
					items = []any{secret}
				}
				if kind == "pods" && scenario == "unexpected pod" {
					items = []any{resource("foreign")}
				}
				if kind == "objectstores.barmancloud.cnpg.io" && created["ObjectStore"] != nil {
					items = []any{created["ObjectStore"]}
				}
				return json.Marshal(object{"items": items})
			}
			if strings.Contains(joined, "--namespace="+ns) {
				if kind == "objectstores.barmancloud.cnpg.io" {
					return json.Marshal(created["ObjectStore"])
				}
				if kind == "cluster.postgresql.cnpg.io" {
					return json.Marshal(created["Cluster"])
				}
			}
			if kind == "cluster.postgresql.cnpg.io" || kind == "objectstores.barmancloud.cnpg.io" || kind == "backups.postgresql.cnpg.io" {
				return original.run(ctx, input, program, args...)
			}
		}
		if args[2] == "create" {
			var desired object
			if json.Unmarshal(input, &desired) != nil {
				t.Fatal("invalid create payload")
			}
			kind := text(desired, "kind")
			if kind != "ObjectStore" && kind != "Cluster" {
				t.Fatal("unexpected creation family")
			}
			response, e := cloneRecoverySpec(desired)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "Cluster" {
				response["spec"] = admitted["spec"]
			}
			if strings.Contains(joined, "--dry-run=server") {
				if scenario == "unsafe admission" {
					response["spec"].(object)["retentionPolicy"] = "1d"
				}
				return json.Marshal(response)
			}
			var persisted cnpgProofRecord
			if json.Unmarshal([]byte(text(cm, "data", "record")), &persisted) != nil || persisted.Admission == nil || persisted.Storage == nil || persisted.Phase != map[string]string{"ObjectStore": "objectstore-requested", "Cluster": "cluster-requested"}[kind] {
				t.Fatal("create lacks durable admission and intent")
			}
			creates++
			if scenario == "uncertain" {
				return nil, errors.New("uncertain API response")
			}
			response["metadata"].(object)["uid"] = map[string]string{"ObjectStore": "55555555-5555-4555-8555-555555555555", "Cluster": "44444444-4444-4444-8444-444444444444"}[kind]
			response["metadata"].(object)["resourceVersion"] = "10"
			if scenario == "changed actual admission" {
				response["spec"].(object)["retentionPolicy"] = "1d"
			}
			created[kind] = response
			return json.Marshal(response)
		}
		return delegate(ctx, input, program, args...)
	}
	activation.source, activation.owner = p.record.Source, p.record.Run
	storageRead := activation.c.run
	activation.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if scenario == "native V2 storage unavailable" && program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "nodes.longhorn.io" {
			return nil, errors.New("native V2 storage unavailable")
		}
		var persisted cnpgProofRecord
		if json.Unmarshal([]byte(text(cm, "data", "record")), &persisted) != nil {
			t.Fatal("invalid fixture journal")
		}
		if persisted.Phase == "objectstore-requested" {
			if scenario == "namespace replaced during storage" {
				namespaceReplacedDuringStorage = true
			}
			if scenario == "storage lost after intent" {
				for _, n := range storageAPI.s.storageNodes {
					if disk, ok := value(n, "status", "diskStatus", "longhorn-v2").(object); ok {
						disk["storageAvailable"] = float64(0)
					}
				}
			}
			if scenario == "activation replaced after intent" {
				storageAPI.record["metadata"].(object)["uid"] = "replaced-activation-journal"
			}
		}
		return storageRead(ctx, input, program, args...)
	}
	p.storage = activation
	return p, cnpgPin, barmanPin, &creates
}

// TestRecoveryDatabaseCreationPersistsAdmissionAndUIDsBeforeProceeding exercises the entire ordered write boundary.
func TestRecoveryDatabaseCreationPersistsAdmissionAndUIDsBeforeProceeding(t *testing.T) {
	p, cnpgPin, barmanPin, creates := databaseCreationFixture(t, "confirmed")
	if _, err := p.databaseAdmissionEnvironment(context.Background(), cnpgPin, barmanPin); err != nil {
		t.Fatal("composed environment fixture:", err)
	}
	if err := p.rebindOriginalArchive(t.Context()); err != nil {
		t.Fatal("composed archive fixture:", err)
	}
	if err := p.verifyDatabaseCreationNamespace(t.Context(), 2); err != nil {
		t.Fatal("composed namespace fixture:", err)
	}
	if err := p.verifyDatabaseCreationPolicy(t.Context()); err != nil {
		t.Fatal("composed isolation fixture:", err)
	}
	storage, err := p.qualifyDatabaseStorage(t.Context())
	if err != nil {
		t.Fatal("composed storage fixture:", err)
	}
	if err := p.verifyRecoveryHostCapacity(t.Context(), storage); err != nil {
		t.Fatal("composed host fixture:", err)
	}
	for _, kind := range []string{"ObjectStore", "Cluster"} {
		if _, err := p.createDatabaseResourceWithSchemas(context.Background(), kind, cnpgPin, barmanPin); err != nil {
			t.Fatal(kind, err)
		}
	}
	if *creates != 2 || p.record.Phase != "cluster-created" || len(p.record.Resources) != 5 || p.record.Resources["Cluster"].UID != "44444444-4444-4444-8444-444444444444" || p.record.Storage == nil || p.record.Storage.Activation.UID != "activation-journal-uid" {
		t.Fatal("created recovery database was not checkpointed")
	}
}

// TestRecoveryDatabaseCreationRefusesUnverifiedAdmissionAndCannotReplayUnknownWrites guards no-adoption and no-retry.
func TestRecoveryDatabaseCreationRefusesUnverifiedAdmissionAndCannotReplayUnknownWrites(t *testing.T) {
	for _, scenario := range []string{"unsafe admission", "uncertain", "foreign secret", "unexpected pod", "source key changed", "main advanced after intent", "namespace replaced after intent", "changed actual admission"} {
		t.Run(scenario, func(t *testing.T) {
			p, cnpgPin, barmanPin, creates := databaseCreationFixture(t, scenario)
			if _, err := p.createDatabaseResourceWithSchemas(context.Background(), "ObjectStore", cnpgPin, barmanPin); err == nil {
				t.Fatal("unverified database resource accepted")
			}
			want := 0
			if scenario == "uncertain" || scenario == "changed actual admission" {
				want = 1
			}
			if *creates != want {
				t.Fatal("unexpected data resource create")
			}
			if scenario == "uncertain" {
				if _, err := p.createDatabaseResourceWithSchemas(context.Background(), "ObjectStore", cnpgPin, barmanPin); err == nil || *creates != want {
					t.Fatal("uncertain data resource create replayed")
				}
			}
			if scenario == "changed actual admission" {
				if p.record.Resources["ObjectStore"].UID != "55555555-5555-4555-8555-555555555555" {
					t.Fatal("created UID lost after failed specification binding")
				}
				if _, err := p.createDatabaseResourceWithSchemas(context.Background(), "Cluster", cnpgPin, barmanPin); err == nil || *creates != 1 {
					t.Fatal("a database followed an unsafe created archive reference")
				}
			}
		})
	}
}

// TestRecoveryDatabaseCreationUsesOnlyTheProtectedReviewedSchemas checks the production caller boundary.
func TestRecoveryDatabaseCreationUsesOnlyTheProtectedReviewedSchemas(t *testing.T) {
	p, _, _, creates := databaseCreationFixture(t, "confirmed")
	if _, err := p.createDatabaseResource(context.Background(), "ObjectStore"); err == nil || *creates != 0 {
		t.Fatal("synthetic schemas satisfied the compiled production pins")
	}
	p.c.context = "foreign"
	p.c.run = func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("foreign context reached a command")
		return nil, nil
	}
	if _, err := p.createDatabaseResource(context.Background(), "ObjectStore"); err == nil {
		t.Fatal("foreign context accepted")
	}
}
