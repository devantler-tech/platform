package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestCurrentMigrationDatabaseRoster(t *testing.T) {
	want := map[cnpgSource]bool{{"observability", "coroot-db"}: true, {"umami", "umami-db"}: true, {"wedding-app", "wedding-db"}: true, {"ate-system", "substrate-db"}: true, {"kagent", "kagent-db"}: true, {"customer-identity", "customer-identity-db"}: true, {"business-site", "business-site-portal-db"}: true, {"business-site-dev", "portal-preview-db"}: true}
	for _, source := range cnpgSources() {
		if !want[source] {
			t.Fatal("unexpected or duplicated database owner", source)
		}
		delete(want, source)
	}
	if len(want) != 0 {
		t.Fatal("migration omits current database owners", want)
	}
}

func TestOlderPostRequestDatabaseJournalsKeepTheirOriginalScope(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, phase := range []string{"reboot-requested", "returning", "recovered"} {
			f, h := hostRebootAPI(t)
			r, err := h.initialize(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			r.Version, r.Phase = version, phase
			for owner, profile := range hostDatabases {
				if profile.sinceVersion > version {
					delete(r.Databases, owner)
				}
			}
			if phase != "reboot-requested" {
				r.RecoveredBootID = "12345678-1234-4234-8234-000000000099"
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			valueObject(f.base.cm, "data")["record"] = string(raw)
			_, read, err := h.read(context.Background())
			if err != nil || !reflect.DeepEqual(r, read) {
				t.Fatal("historical recovery receipt changed scope", version, phase, err)
			}
			r.Version = hostRebootRecordVersion
			if r.validate(h.target) == nil {
				t.Fatal("an old receipt claimed the seven-owner protection without binding it")
			}
		}
	}
}

func TestAddedDatabaseOwnersBlockDisruptionOnRecoveryOrReplacement(t *testing.T) {
	for _, index := range []int{5, 6} {
		for _, fault := range []string{"recovering", "replaced cluster", "replaced system"} {
			f, h := hostRebootAPI(t)
			if _, err := h.initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			db := f.databases[index]
			switch fault {
			case "recovering":
				valueObject(db, "status")["readyInstances"] = float64(1)
			case "replaced cluster":
				valueObject(db, "metadata")["uid"] = "replacement"
			case "replaced system":
				valueObject(db, "status")["systemID"] = "90000000"
			}
			if err := h.run(context.Background()); err == nil || f.drains != 0 || f.reboots != 0 {
				t.Fatal("added database evidence did not hold the worker", index, fault, err)
			}
		}
	}
}

func TestCurrentDatabaseWorkflowUsesFixedChoicesAndNativeMainGuard(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/prepare-longhorn-v2-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			WorkflowDispatch struct {
				Inputs map[string]struct{ Options []string } `json:"inputs"`
			} `json:"workflow_dispatch"`
		} `json:"on"`
		Jobs map[string]struct{ Steps []struct{ Name, Run string } }
	}
	if err = yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	choices := map[string]bool{}
	for _, choice := range workflow.On.WorkflowDispatch.Inputs["database"].Options {
		if choices[choice] {
			t.Fatal("duplicate database choice")
		}
		choices[choice] = true
	}
	sources := cnpgSources()
	if len(choices) != len(sources) {
		t.Fatal("workflow choices differ from the database observation roster")
	}
	for _, source := range sources {
		if !choices[source.Name] {
			t.Fatal("workflow omits a compiled owner", source.Name)
		}
	}
	script := ""
	for _, step := range workflow.Jobs["prepare"].Steps {
		if step.Name == "Require confirmed first attempt on main" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("native workflow guard is missing")
	}
	for _, name := range []string{"customer-identity-db", "business-site-portal-db", "portal-preview-db"} {
		for _, fault := range []string{"", "other database", "other node", "other ref", "later attempt", "wrong confirmation"} {
			env := map[string]string{"PATH": os.Getenv("PATH"), "DISPATCH_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "LONGHORN_V2_OPERATION": "observe-database", "LONGHORN_V2_NODE": "", "LONGHORN_V2_DATABASE": name, "LONGHORN_V2_CONFIRM": "observe-longhorn-v2-database-" + name}
			switch fault {
			case "other database":
				env["LONGHORN_V2_DATABASE"] += "-other"
			case "other node":
				env["LONGHORN_V2_NODE"] = "prod-worker-1"
			case "other ref":
				env["DISPATCH_REF"] = "refs/heads/feature"
			case "later attempt":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "wrong confirmation":
				env["LONGHORN_V2_CONFIRM"] = "prepare-longhorn-v2-prod-worker-1"
			}
			command := exec.Command("bash", "-c", script)
			for key, value := range env {
				command.Env = append(command.Env, key+"="+value)
			}
			if err = command.Run(); (err == nil) != (fault == "") {
				t.Fatalf("native database dispatch mismatch name=%s fault=%s err=%v", name, fault, err)
			}
		}
	}
}

func TestFiveDatabasePreRebootJournalBindsNewOwnersBeforeDrain(t *testing.T) {
	f, h := hostRebootAPI(t)
	r, err := h.initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.Version = 2
	delete(r.Databases, "customer-identity/customer-identity-db")
	delete(r.Databases, "business-site/business-site-portal-db")
	if r.validate(h.target) != nil || len(r.Databases) != 5 {
		t.Fatal("the original five-owner journal cannot be read")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	valueObject(f.base.cm, "data")["record"] = string(raw)
	f.afterDrain = func() {
		var current hostRebootRecord
		if json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &current) != nil || current.Version != 3 || len(current.Databases) != 7 {
			t.Fatal("drain began without binding both new owners")
		}
		for owner, identity := range r.Databases {
			if current.Databases[owner] != identity {
				t.Fatal("an original database identity was replaced")
			}
		}
	}
	if err = h.run(context.Background()); err != nil || f.reboots != 1 {
		t.Fatal("qualified prior journal failed to bind the current owners", err)
	}
}
