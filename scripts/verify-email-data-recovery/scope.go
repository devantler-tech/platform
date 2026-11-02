package main

import (
	"errors"
	"regexp"
)

type invocation struct {
	run, revision, namespace string
}

var (
	recoveryRunPattern      = regexp.MustCompile(`^[1-9][0-9]{6,19}$`)
	recoveryRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func (i invocation) permitsMutation(kind, namespace, name string) bool {
	if !recoveryRunPattern.MatchString(i.run) || !recoveryRevisionPattern.MatchString(i.revision) || i.namespace != "email-data-recovery-"+i.run {
		return false
	}
	if namespace == "" {
		return (kind == "Namespace" || kind == "StorageClass") && name == i.namespace
	}
	if namespace == i.namespace {
		switch kind {
		case "Pod":
			return name == "reader"
		case "PersistentVolumeClaim":
			return name == "data"
		case "ConfigMap":
			return name == "datastore"
		case "Secret":
			return name == "storage-key" || name == "test-authentication"
		case "CiliumNetworkPolicy":
			return name == "isolation"
		}
	}
	if namespace == "velero" {
		switch kind {
		case "Backup":
			return name == i.namespace+"-backup"
		case "DataDownload":
			return name == i.namespace+"-download"
		}
	}
	return false
}

func authorizeDataRecovery(env func(string) string) (invocation, error) {
	for key, expected := range map[string]string{
		"GITHUB_EVENT_NAME":           "workflow_dispatch",
		"GITHUB_REF":                  "refs/heads/main",
		"GITHUB_REPOSITORY":           "devantler-tech/platform",
		"GITHUB_WORKFLOW_REF":         "devantler-tech/platform/.github/workflows/verify-email-data-recovery.yaml@refs/heads/main",
		"GITHUB_ACTOR":                "devantler",
		"GITHUB_RUN_ATTEMPT":          "1",
		"RUNNER_ENVIRONMENT":          "github-hosted",
		"EMAIL_DATA_RECOVERY_CONFIRM": "verify-email-data-recovery",
	} {
		if env(key) != expected {
			return invocation{}, errors.New("dispatch")
		}
	}
	run, revision := env("GITHUB_RUN_ID"), env("GITHUB_SHA")
	if !recoveryRunPattern.MatchString(run) || !recoveryRevisionPattern.MatchString(revision) {
		return invocation{}, errors.New("dispatch")
	}
	return invocation{run: run, revision: revision, namespace: "email-data-recovery-" + run}, nil
}
