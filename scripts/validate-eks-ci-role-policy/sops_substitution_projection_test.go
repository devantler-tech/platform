package main

import (
	"strings"
	"testing"
)

const sopsSubstitutionSourceManifest = `apiVersion: v1
kind: Secret
metadata:
  name: variables-cluster
  namespace: flux-system
type: Opaque
stringData:
  cluster_domain: ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]
  replica_count: ENC[AES256_GCM,data:MTI=,iv:aXYy,tag:dGFnMg==,type:int]
  region_encrypted_not: eu-central
sops:
  mac: ENC[AES256_GCM,data:bWFj,iv:aXYz,tag:dGFnMw==,type:str]
  lastmodified: "2026-07-25T10:00:00Z"
  version: 3.13.2
  encrypted_regex: ^(data|stringData)$
  age:
    - recipient: age1productionrecipientaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      enc: |
        -----BEGIN AGE ENCRYPTED FILE-----
        d3JhcHBlZC1rZXktb25l
        -----END AGE ENCRYPTED FILE-----
`

func sopsSourceEntry(t *testing.T, contents string) string {
	t.Helper()
	documents, err := decodeDocuments([]byte(contents))
	if err != nil || len(documents) != 1 {
		t.Fatalf("decode Secret: documents=%d error=%v", len(documents), err)
	}
	entry, err := authorizationSurfaceEntry(identityOf(documents[0]), documents[0])
	if err != nil {
		t.Fatalf("authorizationSurfaceEntry() error = %v", err)
	}
	return entry
}

func TestAuthorizationSurfaceEntryIgnoresSOPSReEncryption(t *testing.T) {
	baseline := sopsSourceEntry(t, sopsSubstitutionSourceManifest)

	reEncrypted := strings.NewReplacer(
		"data:QUJD,iv:aXYx,tag:dGFnMQ==", "data:WFla,iv:bmV3MQ==,tag:bmV3dGFn",
		"data:MTI=,iv:aXYy,tag:dGFnMg==", "data:OTk=,iv:bmV3Mg==,tag:bmV3dGFnMg==",
		"data:bWFj,iv:aXYz", "data:b3RoZXI=,iv:bmV3Mw==",
		`lastmodified: "2026-07-25T10:00:00Z"`, `lastmodified: "2026-09-19T14:00:00Z"`,
		"version: 3.13.2", "version: 3.13.3",
		"d3JhcHBlZC1rZXktb25l", "cmUtd3JhcHBlZC1rZXk=",
	).Replace(sopsSubstitutionSourceManifest)
	if reEncrypted == sopsSubstitutionSourceManifest {
		t.Fatal("fixture did not change: the re-encryption case would pass vacuously")
	}
	if actual := sopsSourceEntry(t, reEncrypted); actual != baseline {
		t.Fatalf("re-encryption with an unchanged key set moved the authorization surface:\n%s\n%s", baseline, actual)
	}
}

func TestAuthorizationSurfaceEntryKeepsSOPSKeySetAndPlaintext(t *testing.T) {
	baseline := sopsSourceEntry(t, sopsSubstitutionSourceManifest)

	mutations := []struct {
		name string
		old  string
		new  string
	}{
		{
			name: "added substitution variable",
			old:  "  region_encrypted_not: eu-central\n",
			new:  "  region_encrypted_not: eu-central\n  extra_var: ENC[AES256_GCM,data:eA==,iv:aXY0,tag:dGFnNA==,type:str]\n",
		},
		{
			name: "removed substitution variable",
			old:  "  replica_count: ENC[AES256_GCM,data:MTI=,iv:aXYy,tag:dGFnMg==,type:int]\n",
			new:  "",
		},
		{name: "renamed substitution variable", old: "  cluster_domain:", new: "  cluster_domains:"},
		{name: "declared plaintext type", old: "type:int]", new: "type:str]"},
		{name: "unencrypted value", old: "eu-central", new: "us-east"},
		{
			name: "value moved out of encryption",
			old:  "ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]",
			new:  "example.com",
		},
		{
			name: "value moved out of encryption as the old placeholder text",
			old:  "ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]",
			new:  "<sops-encrypted:str>",
		},
		{
			name: "value moved out of encryption as null",
			old:  "ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]",
			new:  "null",
		},
		{
			name: "encryption swapped between two keys",
			old: "  cluster_domain: ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]\n" +
				"  replica_count: ENC[AES256_GCM,data:MTI=,iv:aXYy,tag:dGFnMg==,type:int]\n" +
				"  region_encrypted_not: eu-central\n",
			new: "  cluster_domain: ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]\n" +
				"  replica_count: 12\n" +
				"  region_encrypted_not: ENC[AES256_GCM,data:ZXU=,iv:aXY1,tag:dGFnNQ==,type:str]\n",
		},
		{
			name: "partial ciphertext stays exact",
			old:  "ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]",
			new:  "prefix-ENC[AES256_GCM,data:QUJD,iv:aXYx,tag:dGFnMQ==,type:str]",
		},
		{name: "secret identity", old: "name: variables-cluster", new: "name: variables-shadow"},
		{name: "secret type", old: "type: Opaque", new: "type: kubernetes.io/basic-auth"},
		{name: "age recipient replaced", old: "age1productionrecipient", new: "age1someotherrecipient0"},
		{
			name: "age recipient removed",
			old:  "  age:\n    - recipient: age1productionrecipientaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
			new:  "  age:\n    - other: kept\n",
		},
		{name: "encrypted_regex", old: "encrypted_regex: ^(data|stringData)$", new: "encrypted_regex: ^(data)$"},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := strings.Replace(sopsSubstitutionSourceManifest, mutation.old, mutation.new, 1)
			if mutated == sopsSubstitutionSourceManifest {
				t.Fatalf("fixture did not change for %q", mutation.old)
			}
			if actual := sopsSourceEntry(t, mutated); actual == baseline {
				t.Fatal("reviewable change did not move the authorization surface")
			}
		})
	}
}

func TestAuthorizationSurfaceEntryProjectsOnlySOPSSecrets(t *testing.T) {
	withoutMetadata := sopsSubstitutionSourceManifest[:strings.Index(sopsSubstitutionSourceManifest, "sops:\n")]
	asConfigMap := strings.Replace(sopsSubstitutionSourceManifest, "kind: Secret", "kind: ConfigMap", 1)

	for name, manifest := range map[string]string{
		"Secret without SOPS metadata": withoutMetadata,
		"ConfigMap":                    asConfigMap,
	} {
		t.Run(name, func(t *testing.T) {
			baseline := sopsSourceEntry(t, manifest)
			mutated := strings.Replace(manifest, "data:QUJD,iv:aXYx", "data:WFla,iv:bmV3MQ==", 1)
			if mutated == manifest {
				t.Fatal("fixture did not change")
			}
			if actual := sopsSourceEntry(t, mutated); actual == baseline {
				t.Fatal("ciphertext outside a SOPS-encrypted Secret was normalized")
			}
		})
	}
}

func TestSOPSSubstitutionSourceProjectionLeavesDecodedDocumentIntact(t *testing.T) {
	documents, err := decodeDocuments([]byte(sopsSubstitutionSourceManifest))
	if err != nil || len(documents) != 1 {
		t.Fatalf("decode Secret: documents=%d error=%v", len(documents), err)
	}
	before, err := canonicalFingerprint(documents[0])
	if err != nil {
		t.Fatalf("canonicalFingerprint() error = %v", err)
	}
	projected := sopsSubstitutionSourceSurfaceDocument(identityOf(documents[0]), documents[0])
	document, _ := projected["sopsProjectedDocument"].(map[string]any)
	metadata, _ := document["sops"].(map[string]any)
	for _, volatile := range []string{"mac", "lastmodified", "version"} {
		if _, kept := metadata[volatile]; kept {
			t.Fatalf("projection kept the volatile SOPS field %q", volatile)
		}
	}
	groups, _ := metadata["age"].([]any)
	if len(groups) != 1 {
		t.Fatalf("projection dropped the SOPS recipients: %v", metadata)
	}
	group, _ := groups[0].(map[string]any)
	if _, kept := group["enc"]; kept || group["recipient"] == nil {
		t.Fatalf("projection must keep each recipient and drop its wrapped key: %v", group)
	}
	stringData, _ := document["stringData"].(map[string]any)
	if stringData["cluster_domain"] != nil || stringData["replica_count"] != nil ||
		stringData["region_encrypted_not"] != "eu-central" {
		t.Fatalf("projection did not null only the ciphertext: %v", stringData)
	}
	recorded, _ := projected["sopsEncryptedScalars"].([]string)
	if len(recorded) != 2 || recorded[0] != "/stringData/cluster_domain str" ||
		recorded[1] != "/stringData/replica_count int" {
		t.Fatalf("projection did not record the encrypted paths and types: %v", recorded)
	}
	after, err := canonicalFingerprint(documents[0])
	if err != nil {
		t.Fatalf("canonicalFingerprint() error = %v", err)
	}
	if before != after {
		t.Fatal("projection mutated the decoded document the other checks read")
	}
	if !isSOPSEncrypted(documents[0]) {
		t.Fatal("decoded document no longer reads as SOPS-encrypted")
	}
}
