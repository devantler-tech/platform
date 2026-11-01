package main

import "testing"

func confirmedInvocation() map[string]string {
	return map[string]string{
		"GITHUB_EVENT_NAME":           "workflow_dispatch",
		"GITHUB_REF":                  "refs/heads/main",
		"GITHUB_REPOSITORY":           "devantler-tech/platform",
		"GITHUB_WORKFLOW_REF":         "devantler-tech/platform/.github/workflows/verify-email-data-recovery.yaml@refs/heads/main",
		"GITHUB_SHA":                  "1234567890123456789012345678901234567890",
		"GITHUB_ACTOR":                "devantler",
		"GITHUB_RUN_ID":               "12345678901",
		"GITHUB_RUN_ATTEMPT":          "1",
		"RUNNER_ENVIRONMENT":          "github-hosted",
		"EMAIL_DATA_RECOVERY_CONFIRM": "verify-email-data-recovery",
	}
}

func TestConfirmedMainRecoveryGetsASeparateScope(t *testing.T) {
	env := confirmedInvocation()
	scope, err := authorizeDataRecovery(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal("confirmed first-attempt main invocation was refused")
	}
	if scope.run != env["GITHUB_RUN_ID"] || scope.revision != env["GITHUB_SHA"] || scope.namespace != "email-data-recovery-"+scope.run || scope.namespace == "private-mail" || scope.namespace == "velero" {
		t.Fatal("recovery is not bound to a separate invocation scope")
	}
}

func TestOtherDispatchContextsCannotGetARecoveryScope(t *testing.T) {
	for key, value := range map[string]string{
		"GITHUB_EVENT_NAME":           "pull_request",
		"GITHUB_REF":                  "refs/heads/codex/example",
		"GITHUB_REPOSITORY":           "another-owner/platform",
		"GITHUB_WORKFLOW_REF":         "devantler-tech/platform/.github/workflows/other.yaml@refs/heads/main",
		"GITHUB_SHA":                  "not-a-revision",
		"GITHUB_ACTOR":                "unknown-actor",
		"GITHUB_RUN_ID":               "../private-mail",
		"GITHUB_RUN_ATTEMPT":          "2",
		"RUNNER_ENVIRONMENT":          "self-hosted",
		"EMAIL_DATA_RECOVERY_CONFIRM": "",
	} {
		t.Run(key, func(t *testing.T) {
			env := confirmedInvocation()
			env[key] = value
			if _, err := authorizeDataRecovery(func(name string) string { return env[name] }); err == nil {
				t.Fatal("an unconfirmed or foreign dispatch obtained a recovery scope")
			}
		})
	}
}

func TestRecoveryWritesAreLimitedToTheirOwnResources(t *testing.T) {
	env := confirmedInvocation()
	i, err := authorizeDataRecovery(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range [][3]string{
		{"Namespace", "", i.namespace},
		{"StorageClass", "", i.namespace},
		{"Pod", i.namespace, "reader"},
		{"PersistentVolumeClaim", i.namespace, "data"},
		{"ConfigMap", i.namespace, "datastore"},
		{"Secret", i.namespace, "storage-key"},
		{"Secret", i.namespace, "test-authentication"},
		{"CiliumNetworkPolicy", i.namespace, "isolation"},
		{"Backup", "velero", i.namespace + "-backup"},
		{"DataDownload", "velero", i.namespace + "-download"},
	} {
		t.Run(ref[0]+"/"+ref[2], func(t *testing.T) {
			if !i.permitsMutation(ref[0], ref[1], ref[2]) {
				t.Fatal("invocation-owned resource was refused")
			}
		})
	}
	for _, ref := range [][3]string{
		{"PersistentVolumeClaim", "private-mail", "private-mail-data"},
		{"Pod", "private-mail", "private-mail-0"},
		{"Secret", "private-mail", "private-mail-admin"},
		{"Secret", "velero", "velero-repo-credentials"},
		{"Namespace", "", "private-mail"},
		{"Namespace", "", "velero"},
		{"PersistentVolume", "", "pvc-11111111-1111-4111-8111-111111111111"},
		{"ClusterRoleBinding", "", i.namespace},
		{"RoleBinding", i.namespace, "reader"},
		{"Service", i.namespace, "reader"},
		{"Pod", i.namespace, "foreign"},
		{"Secret", i.namespace, "foreign"},
		{"DataDownload", "velero", "another-invocation-download"},
	} {
		t.Run("refuse/"+ref[0]+"/"+ref[1]+"/"+ref[2], func(t *testing.T) {
			if i.permitsMutation(ref[0], ref[1], ref[2]) {
				t.Fatal("recovery can mutate serving, foreign, routing or permission resources")
			}
		})
	}
}

func TestRecoveryKeepsBackupRetentionWithTheNativeController(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	if i.permitsMutation("DeleteBackupRequest", "velero", i.namespace+"-delete") {
		t.Fatal("recovery should not delete the retained backup")
	}
	for _, ref := range [][3]string{{"DeleteBackupRequest", "velero", "another-backup-delete"}, {"Secret", i.namespace, "test-tls"}, {"ConfigMap", "velero", i.namespace + "-backup-policy"}} {
		if i.permitsMutation(ref[0], ref[1], ref[2]) {
			t.Fatal("unused or foreign recovery capability remains allowed")
		}
	}
}
