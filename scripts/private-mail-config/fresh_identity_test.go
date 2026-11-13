package main

import "testing"

// Recovery has no default Role objects. Fresh initialization must give the
// declared mailbox its own effective permissions before any identity cutover.
func TestFreshMailboxDoesNotDependOnNativeRoleObjects(t *testing.T) {
	plan, err := render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	var mailbox map[string]any
	for _, op := range ops {
		if op.Object == "Account" && op.Type == "create" {
			mailbox = op.Value["maintainer"].(map[string]any)
		}
	}
	permissions, ok := mailbox["permissions"].(map[string]any)
	if !ok || permissions["@type"] != "Replace" {
		t.Fatal("fresh mailbox still depends on generated role objects")
	}
	enabled, ok := permissions["enabledPermissions"].(map[string]any)
	if !ok || len(enabled) != 244 {
		t.Fatal("fresh mailbox has no complete bounded permission profile")
	}
	for _, name := range []string{"authenticate", "imapAuthenticate", "emailSend", "emailReceive", "jmapMailboxGet", "sysAppPasswordCreate"} {
		if enabled[name] != true {
			t.Fatalf("fresh mailbox cannot use %s", name)
		}
	}
	for _, name := range []string{"sysAccountUpdate", "sysRoleUpdate", "impersonate", "unlimitedRequests", "liveTracing"} {
		if _, present := enabled[name]; present {
			t.Fatalf("ordinary mailbox received %s", name)
		}
	}
}
