package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestRecoveryCredentialCopyPersistsOnlyIdentityAndHashBeforeCreatingTheSecret exercises the native write boundary.
func TestRecoveryCredentialCopyPersistsOnlyIdentityAndHashBeforeCreatingTheSecret(t *testing.T) {
	for _, scenario := range []string{"confirmed", "uncertain", "source changed", "main advanced", "foreign source", "namespace replaced"} {
		t.Run(scenario, func(t *testing.T) {
			p, patches := recoveryIsolationStageFixture(t, "")
			if err := p.prepareIsolation(context.Background()); err != nil {
				t.Fatal(err)
			}
			basePatches := *patches
			delegate := p.c.run
			original, _, backup := recoveryPlanningFixture(t)
			value(*backup, "spec").(object)["target"] = "primary"
			data := object{}
			for key, plain := range map[string]string{"ACCESS_KEY_ID": "synthetic-access", "SECRET_ACCESS_KEY": "synthetic-secret", "REGION": "auto"} {
				data[key] = base64.StdEncoding.EncodeToString([]byte(plain))
			}
			source := object{"apiVersion": "v1", "kind": "Secret", "metadata": object{"name": p.record.Plan.Source.Credential, "namespace": "umami", "uid": "66666666-6666-4666-8666-666666666666", "resourceVersion": "10"}, "type": "Opaque", "data": data}
			var destination object
			reads, creates, gitReads, namespaceReads := 0, 0, 0, 0
			p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					gitReads++
					if scenario == "main advanced" && gitReads == 3 {
						return []byte(strings.Repeat("b", 40) + "\trefs/heads/main\n"), nil
					}
					return delegate(ctx, input, program, args...)
				}
				if program != "kubectl" || len(args) < 4 {
					return nil, errors.New("unexpected command")
				}
				if args[2] == "get" {
					switch args[3] {
					case "namespace":
						namespaceReads++
						if scenario == "namespace replaced" && namespaceReads == 3 {
							raw, err := delegate(ctx, input, program, args...)
							if err != nil {
								return nil, err
							}
							var changed object
							if json.Unmarshal(raw, &changed) != nil {
								t.Fatal("invalid namespace fixture")
							}
							changed["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
							return json.Marshal(changed)
						}
					case "cluster.postgresql.cnpg.io", "objectstores.barmancloud.cnpg.io", "backups.postgresql.cnpg.io":
						if args[4] != "--output=json" {
							return original.run(ctx, input, program, args...)
						}
					case "secret":
						if args[4] == p.record.Plan.Source.Credential {
							reads++
							if scenario == "source changed" && reads == 2 {
								data["SECRET_ACCESS_KEY"] = base64.StdEncoding.EncodeToString([]byte("changed-synthetic-key"))
							}
							if scenario == "foreign source" {
								source["metadata"].(object)["namespace"] = "wedding-app"
							}
							return json.Marshal(source)
						}
						return json.Marshal(destination)
					}
				}
				if args[2] == "create" {
					creates++
					cmRaw, err := delegate(ctx, nil, "kubectl", "--context=admin@prod", "--request-timeout=20s", "get", "configmap", "longhorn-v2-db-proof-umami-db", "--output=json", "--namespace=longhorn-system")
					if err != nil {
						t.Fatal(err)
					}
					var cm object
					if json.Unmarshal(cmRaw, &cm) != nil {
						t.Fatal("missing journal")
					}
					record := text(cm, "data", "record")
					var persisted cnpgProofRecord
					if json.Unmarshal([]byte(record), &persisted) != nil || persisted.Phase != "credential-requested" || persisted.Credential == nil || persisted.Credential.DataSHA == "" || strings.Contains(record, data["SECRET_ACCESS_KEY"].(string)) || strings.Contains(record, "synthetic-secret") {
						t.Fatal("credential write lacks a private durable identity witness")
					}
					if scenario == "uncertain" {
						return nil, errors.New("unknown API outcome: " + data["SECRET_ACCESS_KEY"].(string))
					}
					if json.Unmarshal(input, &destination) != nil || text(destination, "kind") != "Secret" || text(destination, "metadata", "name") != "archive-read" || destination["immutable"] != true || len(value(destination, "data").(object)) != 3 {
						t.Fatal("unexpected credential payload")
					}
					destination["metadata"].(object)["uid"] = "77777777-7777-4777-8777-777777777777"
					destination["metadata"].(object)["resourceVersion"] = "10"
					return json.Marshal(destination)
				}
				return delegate(ctx, input, program, args...)
			}
			err := p.copySourceCredential(context.Background())
			if scenario == "confirmed" {
				if err != nil || creates != 1 || *patches != basePatches+2 || p.record.Phase != "credential-created" || p.record.Resources["Secret"].UID == "" {
					t.Fatalf("complete credential copy failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("uncertain credential copy accepted")
			}
			if strings.Contains(err.Error(), data["SECRET_ACCESS_KEY"].(string)) {
				t.Fatal("credential value escaped its error boundary")
			}
			wantCreates := 0
			if scenario == "uncertain" {
				wantCreates = 1
			}
			if creates != wantCreates {
				t.Fatal("unexpected native credential creation")
			}
			before := creates
			if scenario != "foreign source" {
				if err = p.copySourceCredential(context.Background()); err == nil || creates != before {
					t.Fatal("uncertain credential creation replayed")
				}
			}
		})
	}
}
