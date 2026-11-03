package backupcatalogue

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func bindingFixture(t *testing.T, namespace string) (Policy, DeploymentDocuments) {
	t.Helper()
	encode := func(v any) []byte {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	metadata := func(name string) map[string]any {
		return map[string]any{"name": name, "namespace": "velero", "uid": name + "-fixture-uid", "resourceVersion": "123", "creationTimestamp": "2026-06-01T00:00:00Z"}
	}
	p := Policy{Endpoint: "https://" + strings.Repeat("a", 32) + ".r2.cloudflarestorage.com", Bucket: "fixture-backups", Prefix: "velero/prod/", MaxRequests: 1024, MaxBytes: 64 << 20, MaxResponseBytes: 16 << 20}
	return p, DeploymentDocuments{
		Location:         encode(map[string]any{"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation", "metadata": metadata("default"), "spec": map[string]any{"provider": "aws", "accessMode": "ReadWrite", "objectStorage": map[string]any{"bucket": p.Bucket, "prefix": "velero/prod"}, "config": map[string]string{"s3Url": p.Endpoint, "region": "auto", "s3ForcePathStyle": "true", "checksumAlgorithm": ""}}, "status": map[string]any{"phase": "Available"}}),
		Repository:       encode(map[string]any{"apiVersion": "velero.io/v1", "kind": "BackupRepository", "metadata": metadata(namespace + "-default-kopia"), "spec": map[string]any{"backupStorageLocation": "default", "repositoryType": "kopia", "volumeNamespace": namespace}, "status": map[string]any{"phase": "Ready"}}),
		CloudSecret:      encode(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata("velero-r2-credentials"), "type": "Opaque", "data": map[string]string{"cloud": base64.StdEncoding.EncodeToString([]byte("[default]\naws_access_key_id=fixture-access\naws_secret_access_key=fixture-secret\n"))}}),
		RepositorySecret: encode(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata("velero-repo-credentials"), "type": "Opaque", "data": map[string]string{"repository-password": base64.StdEncoding.EncodeToString([]byte(" fixture-password\n"))}}),
	}
}

func TestDeploymentBindingPreservesExactVeleroScope(t *testing.T) {
	for _, namespace := range []string{"wedding-app", "actual-budget"} {
		t.Run(namespace, func(t *testing.T) {
			policy, docs := bindingFixture(t, namespace)
			binding, err := bindDeployment(namespace, policy, docs)
			if err != nil {
				t.Fatal(err)
			}
			if binding.policy.Prefix != "velero/prod/kopia/"+namespace+"/" || binding.accessKey != "fixture-access" || binding.secretKey != "fixture-secret" || binding.password != "fixture-password" || len(binding.state) != 4 {
				t.Fatal("wrong repository or credential binding")
			}
		})
	}
}

func changeBinding(t *testing.T, data []byte, change func(map[string]any)) []byte {
	t.Helper()
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		t.Fatal("fixture decode")
	}
	change(value)
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDeploymentBindingRejectsWrongOrAmbiguousEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*DeploymentDocuments)
	}{
		{"foreign-repository", func(d *DeploymentDocuments) {
			d.Repository = changeBinding(t, d.Repository, func(v map[string]any) { v["spec"].(map[string]any)["volumeNamespace"] = "foreign" })
		}},
		{"foreign-location", func(d *DeploymentDocuments) {
			d.Repository = changeBinding(t, d.Repository, func(v map[string]any) { v["spec"].(map[string]any)["backupStorageLocation"] = "other" })
		}},
		{"repository-not-ready", func(d *DeploymentDocuments) {
			d.Repository = changeBinding(t, d.Repository, func(v map[string]any) { v["status"].(map[string]any)["phase"] = "NotReady" })
		}},
		{"repository-deleting", func(d *DeploymentDocuments) {
			d.Repository = changeBinding(t, d.Repository, func(v map[string]any) { v["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-01T00:00:00Z" })
		}},
		{"wrong-backend", func(d *DeploymentDocuments) {
			d.Repository = changeBinding(t, d.Repository, func(v map[string]any) { v["spec"].(map[string]any)["repositoryType"] = "restic" })
		}},
		{"foreign-credentials", func(d *DeploymentDocuments) {
			d.Location = changeBinding(t, d.Location, func(v map[string]any) {
				v["spec"].(map[string]any)["credential"] = map[string]any{"name": "foreign", "key": "cloud"}
			})
		}},
		{"bucket-drift", func(d *DeploymentDocuments) {
			d.Location = changeBinding(t, d.Location, func(v map[string]any) {
				v["spec"].(map[string]any)["objectStorage"].(map[string]any)["bucket"] = "foreign-backups"
			})
		}},
		{"prefix-drift", func(d *DeploymentDocuments) {
			d.Location = changeBinding(t, d.Location, func(v map[string]any) {
				v["spec"].(map[string]any)["objectStorage"].(map[string]any)["prefix"] = "velero/../foreign"
			})
		}},
		{"endpoint-drift", func(d *DeploymentDocuments) {
			d.Location = changeBinding(t, d.Location, func(v map[string]any) {
				v["spec"].(map[string]any)["config"].(map[string]any)["s3Url"] = "https://foreign.invalid"
			})
		}},
		{"foreign-config", func(d *DeploymentDocuments) {
			d.Location = changeBinding(t, d.Location, func(v map[string]any) {
				v["spec"].(map[string]any)["config"].(map[string]any)["credentialsFile"] = "/ambient"
			})
		}},
		{"secret-missing-uid", func(d *DeploymentDocuments) {
			d.CloudSecret = changeBinding(t, d.CloudSecret, func(v map[string]any) { delete(v["metadata"].(map[string]any), "uid") })
		}},
		{"secret-foreign-namespace", func(d *DeploymentDocuments) {
			d.CloudSecret = changeBinding(t, d.CloudSecret, func(v map[string]any) { v["metadata"].(map[string]any)["namespace"] = "foreign" })
		}},
		{"missing-password", func(d *DeploymentDocuments) {
			d.RepositorySecret = changeBinding(t, d.RepositorySecret, func(v map[string]any) { v["data"] = map[string]any{} })
		}},
		{"empty-password", func(d *DeploymentDocuments) {
			d.RepositorySecret = changeBinding(t, d.RepositorySecret, func(v map[string]any) {
				v["data"] = map[string]any{"repository-password": base64.StdEncoding.EncodeToString([]byte(" \n"))}
			})
		}},
		{"placeholder-cloud", func(d *DeploymentDocuments) {
			d.CloudSecret = changeBinding(t, d.CloudSecret, func(v map[string]any) {
				v["data"] = map[string]any{"cloud": base64.StdEncoding.EncodeToString([]byte("[default]\naws_access_key_id=PLACEHOLDER\naws_secret_access_key=PLACEHOLDER\n"))}
			})
		}},
		{"ambient-profile", func(d *DeploymentDocuments) {
			d.CloudSecret = changeBinding(t, d.CloudSecret, func(v map[string]any) {
				v["data"] = map[string]any{"cloud": base64.StdEncoding.EncodeToString([]byte("[other]\naws_access_key_id=fixture-access\naws_secret_access_key=fixture-secret\n"))}
			})
		}},
		{"session-token", func(d *DeploymentDocuments) {
			d.CloudSecret = changeBinding(t, d.CloudSecret, func(v map[string]any) {
				v["data"] = map[string]any{"cloud": base64.StdEncoding.EncodeToString([]byte("[default]\naws_access_key_id=fixture-access\naws_secret_access_key=fixture-secret\naws_session_token=extra\n"))}
			})
		}},
		{"duplicate-field", func(d *DeploymentDocuments) {
			d.Repository = []byte(strings.Replace(string(d.Repository), `"kind":"BackupRepository"`, `"kind":"BackupRepository","kind":"BackupRepository"`, 1))
		}},
		{"case-alias", func(d *DeploymentDocuments) {
			d.Repository = []byte(strings.Replace(string(d.Repository), `"volumeNamespace"`, `"VolumeNamespace"`, 1))
		}},
		{"trailing-json", func(d *DeploymentDocuments) { d.Repository = append(d.Repository, []byte(" {}")...) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, docs := bindingFixture(t, "wedding-app")
			test.edit(&docs)
			binding, err := bindDeployment("wedding-app", policy, docs)
			if err == nil || binding.password != "" || binding.secretKey != "" {
				t.Fatal("accepted incomplete or wrong binding")
			}
		})
	}
	policy, docs := bindingFixture(t, "wedding-app")
	if _, err := bindDeployment("foreign", policy, docs); err == nil {
		t.Fatal("foreign consumer")
	}
}

func TestDeploymentBindingDetectsCredentialOrIdentityChanges(t *testing.T) {
	for _, changed := range []string{"repository password", "fixture-access", "fixture-secret"} {
		t.Run(changed, func(t *testing.T) {
			p, docs := bindingFixture(t, "wedding-app")
			before, err := bindDeployment("wedding-app", p, docs)
			if err != nil {
				t.Fatal(err)
			}
			if changed == "repository password" {
				docs.RepositorySecret = changeBinding(t, docs.RepositorySecret, func(v map[string]any) {
					v["data"] = map[string]any{"repository-password": base64.StdEncoding.EncodeToString([]byte("rotated-password"))}
				})
			} else {
				docs.CloudSecret = changeBinding(t, docs.CloudSecret, func(v map[string]any) {
					data := v["data"].(map[string]any)
					cloud, decodeErr := base64.StdEncoding.DecodeString(data["cloud"].(string))
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
					data["cloud"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(cloud), changed, changed+"-rotated", 1)))
				})
			}
			after, err := bindDeployment("wedding-app", p, docs)
			if err != nil || sameDeployment(before, after) {
				t.Fatal("credential change not bound")
			}
		})
	}
}

func TestBindingKeepsEachObjectIdentityWhileAllowingResourceVersionChurn(t *testing.T) {
	for index, name := range []string{"location", "repository", "cloud credentials", "repository credentials"} {
		t.Run(name, func(t *testing.T) {
			policy, docs := bindingFixture(t, "wedding-app")
			before, err := bindDeployment("wedding-app", policy, docs)
			if err != nil {
				t.Fatal(err)
			}
			target := []*[]byte{&docs.Location, &docs.Repository, &docs.CloudSecret, &docs.RepositorySecret}[index]
			*target = changeBinding(t, *target, func(v map[string]any) {
				v["metadata"].(map[string]any)["resourceVersion"] = "124"
			})
			heartbeat, err := bindDeployment("wedding-app", policy, docs)
			if err != nil || !sameDeployment(before, heartbeat) {
				t.Fatal("resource version alone invalidated an unchanged binding")
			}
			*target = changeBinding(t, *target, func(v map[string]any) {
				v["metadata"].(map[string]any)["uid"] = "replacement-fixture-uid"
			})
			replaced, err := bindDeployment("wedding-app", policy, docs)
			if err != nil || sameDeployment(replaced, before) {
				t.Fatal("replacement identity was not bound")
			}
			*target = changeBinding(t, *target, func(v map[string]any) {
				delete(v["metadata"].(map[string]any), "resourceVersion")
			})
			if _, err := bindDeployment("wedding-app", policy, docs); err == nil {
				t.Fatal("an incomplete native response qualified")
			}
		})
	}
}
