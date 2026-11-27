package main

import "testing"

// Missing native V2 storage must prevent the first database resource write,
// even when the archive, isolation policy and operator admission all qualify.
func TestRecoveryDatabaseCreationRequiresNativeV2Storage(t *testing.T) {
	p, cnpgPin, barmanPin, creates := databaseCreationFixture(t, "native V2 storage unavailable")
	if _, err := p.createDatabaseResourceWithSchemas(t.Context(), "ObjectStore", cnpgPin, barmanPin); err == nil || *creates != 0 {
		t.Fatalf("database creation proceeded without native V2 storage: creates=%d err=%v", *creates, err)
	}
}

func TestRecoveryDatabaseCreationRequalifiesStorageAfterIntent(t *testing.T) {
	for _, scenario := range []string{"storage lost after intent", "activation replaced after intent", "worker lost capacity after intent"} {
		t.Run(scenario, func(t *testing.T) {
			p, cnpgPin, barmanPin, creates := databaseCreationFixture(t, scenario)
			if _, err := p.createDatabaseResourceWithSchemas(t.Context(), "ObjectStore", cnpgPin, barmanPin); err == nil || *creates != 0 {
				t.Fatalf("database creation followed changed storage: creates=%d err=%v", *creates, err)
			}
			if p.record.Phase != "objectstore-requested" || p.record.Storage == nil {
				t.Fatal("storage drift lost the durable intent or original storage witness")
			}
		})
	}
}

func TestRecoveryDatabaseCreationRefusesNamespaceReplacementDuringStorage(t *testing.T) {
	p, cnpgPin, barmanPin, creates := databaseCreationFixture(t, "namespace replaced during storage")
	if _, err := p.createDatabaseResourceWithSchemas(t.Context(), "ObjectStore", cnpgPin, barmanPin); err == nil || *creates != 0 {
		t.Fatalf("database creation used a namespace replaced during storage qualification: creates=%d err=%v", *creates, err)
	}
}

func TestRecoveryJournalPreservesRecordedStorageIdentity(t *testing.T) {
	for _, change := range []string{"unchanged", "drop", "replace"} {
		t.Run(change, func(t *testing.T) {
			p, cnpgPin, barmanPin, _ := databaseCreationFixture(t, "confirmed")
			if _, err := p.createDatabaseResourceWithSchemas(t.Context(), "ObjectStore", cnpgPin, barmanPin); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "drop":
				p.record.Storage = nil
			case "replace":
				p.record.Storage.Activation.UID = "replaced-activation-journal"
			}
			p.record.Phase = "cluster-requested"
			err := p.save(t.Context())
			if change == "unchanged" && err != nil {
				t.Fatal("the unchanged storage witness prevented a valid transition:", err)
			}
			if change != "unchanged" && err == nil {
				t.Fatal("an established recovery storage identity was changed")
			}
		})
	}
}
