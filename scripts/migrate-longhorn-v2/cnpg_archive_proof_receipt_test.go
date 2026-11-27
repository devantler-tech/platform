package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCNPGArchiveProofReceiptBindsOnlyTheRecoveredClaimAndBackup(t *testing.T) {
	record := recoveredArchiveReceiptFixture(t)
	expected := *record.RestoreReceipt
	for _, fault := range []string{"", "receipt absent", "wrong backup", "wrong claim", "foreign volume", "duplicate identity", "missing evidence", "unconfirmed phase"} {
		t.Run(fault, func(t *testing.T) {
			candidate, receipt := record, expected
			candidate.RestoreReceipt = &receipt
			switch fault {
			case "receipt absent":
				candidate.RestoreReceipt = nil
			case "wrong backup":
				receipt.Backup.UID = expected.Consumer.UID
			case "wrong claim":
				receipt.Claim.UID = "99999999-9999-4999-8999-999999999999"
			case "foreign volume":
				receipt.Volume.Namespace = "another-namespace"
			case "duplicate identity":
				receipt.Volume.UID = expected.PV.UID
			case "missing evidence":
				receipt.EvidenceSHA256 = ""
			case "unconfirmed phase":
				candidate.Phase = "cluster-created"
			}
			raw, err := encodeCNPGProofRecord(candidate)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeCNPGProofRecord([]byte(raw))
			if (err == nil) != (fault == "") || err == nil && (got.RestoreReceipt == nil || *got.RestoreReceipt != expected) {
				t.Fatalf("unexpected recovered archive receipt result: %v", err)
			}
		})
	}
}

func recoveredArchiveReceiptFixture(t *testing.T) cnpgProofRecord {
	t.Helper()
	f := newRecoveredStorageFixture(t)
	storage, err := f.bind()
	if err != nil {
		t.Fatal(err)
	}
	record := f.p.record
	record.Backup.UID = "98765432-4444-4444-8444-444444444444"
	record.Plan.Source.BackupUID = record.Backup.UID
	record.Phase, record.RestoreVerified = "restore-verified", true
	record.RestoreReceipt = &cnpgArchiveProofReceipt{Backup: record.Backup, Cluster: storage.Cluster, Consumer: storage.Pod, Claim: storage.Claim,
		PV: storage.PV, Volume: storage.Volume, EvidenceSHA256: strings.Repeat("a", 64)}
	return record
}

func TestCNPGProofRetiresOnlyItsOriginalTemporaryArchiveCredential(t *testing.T) {
	for _, scenario := range []string{"confirmed", "replacement", "ambiguous delete"} {
		t.Run(scenario, func(t *testing.T) {
			record := recoveredArchiveReceiptFixture(t)
			id := record.Resources["Secret"]
			secret := object{
				"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
				"metadata": object{"uid": id.UID, "namespace": id.Namespace, "name": id.Name, "resourceVersion": "58",
					"labels": object{"storage.platform.devantler.tech/recovery-run": record.Run, "app.kubernetes.io/managed-by": "github-actions"}},
				"data": object{"ACCESS_KEY_ID": "YQ==", "SECRET_ACCESS_KEY": "Yg==", "REGION": "Yw=="},
			}
			checksum, err := recoveryCredentialData(secret)
			if err != nil {
				t.Fatal(err)
			}
			record.Credential.DataSHA = checksum
			journal := ownedCNPGJournalFixture(t, record)
			var phases []string
			deleted := false
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" && slices.Equal(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"}) {
					return []byte(record.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || args[0] != "--context=admin@prod" {
					t.Fatal("credential retirement left the reviewed production client", program, args)
				}
				switch args[2] {
				case "get":
					if args[3] == "configmap" && args[4] == text(journal, "metadata", "name") {
						return json.Marshal(journal)
					}
					if input != nil || args[3] != "secret" || args[4] != "archive-read" || !slices.Contains(args, "--namespace="+id.Namespace) {
						t.Fatal("retirement tried to read a source Secret", args)
					}
					if deleted {
						return nil, nil
					}
					if scenario == "replacement" && len(phases) == 1 {
						valueObject(secret, "metadata")["uid"] = "99999999-9999-4999-8999-999999999999"
					}
					return json.Marshal(secret)
				case "patch":
					var changes []object
					if args[3] != "configmap" || args[4] != text(journal, "metadata", "name") || json.Unmarshal(input, &changes) != nil ||
						len(changes) != 3 || text(changes[2], "path") != "/data/record" {
						t.Fatal("the credential intent escaped the original proof journal")
					}
					raw := text(changes[2], "value")
					next, err := decodeCNPGProofRecord([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					phases = append(phases, next.Phase)
					valueObject(journal, "data")["record"] = raw
					return nil, nil
				case "delete":
					var options object
					if deleted || !slices.Equal(phases, []string{cnpgArchiveCredentialDeleteRequested}) ||
						args[3] != "--raw=/api/v1/namespaces/"+id.Namespace+"/secrets/archive-read" ||
						json.Unmarshal(input, &options) != nil || text(options, "preconditions", "uid") != id.UID ||
						text(options, "preconditions", "resourceVersion") != "58" {
						t.Fatal("the credential deletion was unbound or replayed", args)
					}
					deleted = true
					if scenario == "ambiguous delete" {
						return nil, errors.New("fixture did not confirm deletion")
					}
					return nil, nil
				default:
					t.Fatal("unexpected capability during credential retirement", args)
				}
				return nil, nil
			}}
			p := cnpgProofRun{c: c, record: record, journalUID: text(journal, "metadata", "uid")}
			err = p.retireArchiveReadCredential(t.Context())
			want := []string{cnpgArchiveCredentialDeleteRequested}
			if scenario == "confirmed" {
				want = append(want, cnpgArchiveCredentialRetired)
			}
			if (err == nil) != (scenario == "confirmed") || !slices.Equal(phases, want) || deleted != (scenario != "replacement") {
				t.Fatalf("unexpected credential retirement outcome: phases=%v deleted=%t err=%v", phases, deleted, err)
			}
			if err := p.retireArchiveReadCredential(t.Context()); err == nil {
				t.Fatal("credential retirement replayed a terminal or ambiguous write")
			}
		})
	}
}
