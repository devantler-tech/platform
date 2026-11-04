package celapplicability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnerCensusRefusesExtraOccurrencesAndChangedOwners(t *testing.T) {
	for _, mutation := range []string{"none", "extra owner", "extra sum", "duplicate sum", "changed version", "changed module", "replacement", "missing owner", "workspace"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]trackedFile{}
			for _, owner := range owners {
				files[owner.Directory+"/go.mod"] = trackedFile{Data: []byte("module " + owner.Module + "\ngo 1.27.3\nrequire " + modulePath + " " + moduleVersion + "\n")}
				files[owner.Directory+"/go.sum"] = trackedFile{Data: []byte(modulePath + " " + moduleVersion + " " + celSum + "\n" + modulePath + " " + moduleVersion + "/go.mod " + celModSum + "\n")}
			}
			mod, sum := owners[0].Directory+"/go.mod", owners[0].Directory+"/go.sum"
			switch mutation {
			case "extra owner":
				files["other/go.mod"] = trackedFile{Data: []byte("module example.test/other\nrequire " + modulePath + " " + moduleVersion + "\n")}
			case "extra sum":
				files["other/go.sum"] = files[sum]
			case "duplicate sum":
				file := files[sum]
				file.Data = append(append([]byte(nil), file.Data...), file.Data...)
				files[sum] = file
			case "changed version":
				file := files[mod]
				file.Data = []byte(strings.ReplaceAll(string(file.Data), moduleVersion, "v0.29.0"))
				files[mod] = file
			case "changed module":
				file := files[mod]
				file.Data = []byte(strings.ReplaceAll(string(file.Data), owners[0].Module, "example.test/changed"))
				files[mod] = file
			case "replacement":
				file := files[mod]
				file.Data = append(file.Data, []byte("replace "+modulePath+" => /elsewhere\n")...)
				files[mod] = file
			case "missing owner":
				delete(files, mod)
			case "workspace":
				files["go.work"] = trackedFile{Data: []byte("go 1.27.3\n")}
			}
			for name, file := range files {
				full := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, file.Data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkOwners(t.Context(), root, files)
			if (err == nil) != (mutation == "none") {
				t.Fatalf("owner census accepted=%v, mutation=%s: %v", err == nil, mutation, err)
			}
		})
	}
}

func TestIndexMustMatchHEADWithoutLinksOrConflictStages(t *testing.T) {
	const oid = "ce013625030ba8dba906f756967f9e9ca394464a"
	index := []byte("100644 " + oid + " 0\tfile.go\x00")
	tree := []byte("100644 blob " + oid + "\tfile.go\x00")
	if _, err := matchingEntries(index, tree); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{
		"100644 " + oid + " 1\tfile.go\x00",
		"120000 " + oid + " 0\tfile.go\x00",
		"100644 " + oid + " 0\t../file.go\x00",
		"100644 " + oid + " 0\tother.go\x00",
		string(index) + string(index),
	} {
		if _, err := matchingEntries([]byte(changed), tree); err == nil {
			t.Fatal("accepted index/HEAD drift")
		}
	}
}

func TestRefusesNativeReferencesAndIncompleteDeclarations(t *testing.T) {
	if err := inspectNative(sourceFile{Path: "hidden_windows.s", Data: []byte("CALL ext·NativeTypes(SB)\n")}); err == nil {
		t.Fatal("accepted an assembly reference")
	}
	for _, counts := range []map[string]int{{}, {"NativeTypes": 1}, {"NativeTypes": 2, "ParseStructTag": 1}} {
		if err := completeDeclarations(counts); err == nil {
			t.Fatal("accepted missing or duplicate canonical declarations")
		}
	}
}

func TestAdvisoryIdentityAndDatabaseFreshnessStayClosed(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	primary, err := os.ReadFile(filepath.Join("testdata", "GO-2026-6094.json"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := []byte(`{"modified":"2026-11-03T22:01:43Z"}`)
	if _, err := validateAdvisory(fresh, primary, now); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{`{}`, `{"modified":"2026-10-01T00:00:00Z"}`, `{"modified":"2026-11-05T00:00:00Z"}`} {
		if _, err := validateAdvisory([]byte(index), primary, now); err == nil {
			t.Fatal("accepted missing/stale/future advisory database freshness")
		}
	}
	changed := []byte(strings.ReplaceAll(string(primary), "GHSA-gcjh-h69q-9w9g", "GHSA-unreviewed"))
	if _, err := validateAdvisory(fresh, changed, now); err == nil {
		t.Fatal("accepted a changed advisory alias")
	}
}
