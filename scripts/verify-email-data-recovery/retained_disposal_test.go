package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
)

func TestRetainedDisposalCommandRefusesForeignDispatchBeforeAccess(t *testing.T) {
	if os.Getenv("EMAIL_DISPOSAL_CHILD") == "1" {
		os.Args = []string{"verify-email-data-recovery", "--dispose-retained"}
		main()
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRetainedDisposalCommandRefusesForeignDispatchBeforeAccess$")
	// Keep coverage-runtime output separate without weakening exact CLI output.
	command.Env = []string{"EMAIL_DISPOSAL_CHILD=1", "PATH=/nonexistent", "GOCOVERDIR=" + t.TempDir()}
	output, err := command.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || string(output) != "EMAIL_RETAINED_DISPOSAL=UNVERIFIED stage=dispatch cleanup=NOT_STARTED\n" {
		t.Fatalf("disposal command lost refusal boundary: %q", output)
	}
}

func TestRetainedInventoryRefusesForeignGroupWithAllowedKindAndName(t *testing.T) {
	for _, tc := range []struct{ kind, name, resource string }{{"ServiceAccount", "default", "serviceaccounts"}, {"ConfigMap", "kube-root-ca.crt", "configmaps"}, {"NetworkPolicy", "default-deny", "networkpolicies.networking.k8s.io"}, {"Secret", "storage-key", "secrets"}} {
		binding := namespaceinventory.Binding{Kind: tc.kind, Name: tc.name, Resource: tc.resource}
		if !retainedInventoryResource(binding) {
			t.Fatal("standard API resource refused")
		}
		binding.Resource = tc.resource + ".foreign.invalid"
		if retainedInventoryResource(binding) {
			t.Fatal("foreign CRD inherited allowed kind/name authority")
		}
	}
}

// Events from either Event API are skipped as transient; nothing else is, and
// an Event-shaped foreign CRD never inherits that.
func TestRetainedInventorySkipsOnlyNativeEvents(t *testing.T) {
	for _, resource := range []string{"events", "events.events.k8s.io"} {
		if !retainedTransientResource(namespaceinventory.Binding{Kind: "Event", Name: "reader.1", Resource: resource}) {
			t.Fatalf("native Event from %s was not transient", resource)
		}
	}
	for _, binding := range []namespaceinventory.Binding{
		{Kind: "Event", Name: "reader.1", Resource: "events.foreign.invalid"},
		{Kind: "Pod", Name: "reader", Resource: "pods"},
		{Kind: "Secret", Name: "events", Resource: "events"},
	} {
		if retainedTransientResource(binding) {
			t.Fatalf("%s from %s was skipped as transient", binding.Kind, binding.Resource)
		}
	}
}

func retainedDispatch() map[string]string {
	env := confirmedInvocation()
	env["GITHUB_ACTIONS"] = "true"
	env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/dispose-retained-email-copy.yaml@refs/heads/main"
	env["EMAIL_RETAINED_COPY_CONFIRM"] = "dispose-retained-email-copy"
	env["EMAIL_RETAINED_READER_UID"] = "11111111-1111-1111-1111-111111111111"
	env["EMAIL_RETAINED_CLAIM_UID"] = "22222222-2222-2222-2222-222222222222"
	return env
}

func TestRetainedDisposalRefusesWrongDispatchAndLostOutputBeforeAccess(t *testing.T) {
	for _, invalid := range []string{"GITHUB_ACTIONS", "GITHUB_ACTOR", "GITHUB_REF", "GITHUB_WORKFLOW_REF", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "EMAIL_RETAINED_COPY_CONFIRM", "EMAIL_RETAINED_READER_UID", "EMAIL_RETAINED_CLAIM_UID", "lost-output"} {
		t.Run(invalid, func(t *testing.T) {
			env := retainedDispatch()
			var out bytes.Buffer
			var output io.Writer = &out
			if invalid == "lost-output" {
				output = &observationFailingWriter{base: &out}
			} else {
				env[invalid] = "private-input-canary"
			}
			k := &refusedAPI{}
			if runRetainedDisposal(context.Background(), func(key string) string { return env[key] }, k, output) != 2 || k.calls != 0 || strings.Contains(out.String(), "private-input-canary") {
				t.Fatal("unconfirmed disposal accessed cluster or exposed input")
			}
		})
	}
	var out bytes.Buffer
	k := &refusedAPI{}
	env := retainedDispatch()
	if runRetainedDisposal(context.Background(), func(key string) string { return env[key] }, k, &out) != 2 || k.calls != 0 || !strings.Contains(out.String(), "stage=credentials") || strings.Contains(out.String(), "private-provider") {
		t.Fatal("invalid credential envelope reached cluster access or exposed provider")
	}
}

func TestRetainedBackupAcceptsObservedDefaultsAndRefusesExpandedScope(t *testing.T) {
	want := child(backupManifest(retainedRecovery), "spec")
	actual := object{}
	for k, v := range want {
		actual[k] = v
	}
	defaults := object{"csiSnapshotTimeout": "10m0s", "itemOperationTimeout": "4h0m0s", "snapshotMoveData": false, "volumeGroupSnapshotLabelKey": "velero.io/volume-group", "excludedResources": []any{"volumesnapshots.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io"}}
	for k, v := range defaults {
		actual[k] = v
	}
	if !retainedBackupSpec(want, actual) {
		t.Fatal("native defaults refused")
	}
	actual["hooks"] = object{"resources": []any{object{"name": "foreign"}}}
	if retainedBackupSpec(want, actual) {
		t.Fatal("expanded backup scope accepted")
	}
}

func TestRetainedGraphRequiresIndependentIdentitiesAndReviewedContents(t *testing.T) {
	for _, failure := range []string{"", "reader replaced", "claim replaced", "extra key", "wrong key", "wrong config", "unfinished download", "wrong storage class", "wrong executable", "terminating"} {
		t.Run(failure, func(t *testing.T) {
			d, a, ctx := cleanupFixture(t, false)
			readerKey := cleanupKey("Pod", d.i.namespace, "reader")
			claimKey := cleanupKey("PersistentVolumeClaim", d.i.namespace, "data")
			readerUID, claimUID := text(child(a.objects[readerKey], "metadata"), "uid"), text(child(a.objects[claimKey], "metadata"), "uid")
			for _, o := range a.objects {
				if text(o, "kind") == "Secret" {
					data := object{}
					for k, v := range child(o, "stringData") {
						data[k] = base64.StdEncoding.EncodeToString([]byte(v.(string)))
					}
					o["data"] = data
					delete(o, "stringData")
				}
			}
			storageKey := child(a.objects[cleanupKey("Secret", d.i.namespace, "storage-key")], "data")
			storageKey["CRYPTO_KEY_VALUE"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 64)))
			child(a.objects[cleanupKey("Secret", d.i.namespace, "test-authentication")], "data")["credential"] = base64.StdEncoding.EncodeToString([]byte("restore:" + strings.Repeat("b", 64)))
			child(a.objects[readerKey], "metadata")["deletionTimestamp"] = nil
			a.objects[readerKey]["status"] = object{"phase": "Running"}
			download := a.objects[cleanupKey("DataDownload", "velero", d.i.namespace+"-download")]
			child(download, "status")["progress"] = object{"totalBytes": float64(100), "bytesDone": float64(100)}
			switch failure {
			case "reader replaced":
				child(a.objects[readerKey], "metadata")["uid"] = "replacement"
			case "claim replaced":
				child(a.objects[claimKey], "metadata")["uid"] = "replacement"
			case "extra key":
				storageKey["foreign"] = "Zm9yZWlnbg=="
			case "wrong key":
				storageKey["CRYPTO_KEY_CIPHER"] = "Zm9yZWlnbg=="
			case "wrong config":
				child(a.objects[cleanupKey("ConfigMap", d.i.namespace, "datastore")], "data")["config.json"] = "{}"
			case "unfinished download":
				child(download, "status")["phase"] = "InProgress"
			case "wrong storage class":
				a.objects[cleanupKey("StorageClass", "", d.i.namespace)]["reclaimPolicy"] = "Retain"
			case "wrong executable":
				items(child(a.objects[readerKey], "spec"), "containers")[0]["image"] = "foreign"
			case "terminating":
				child(a.objects[readerKey], "metadata")["deletionTimestamp"] = "2026-11-03T00:00:00Z"
			}
			d.refs = map[string]ownedReference{}
			err := d.adoptRetainedGraph(ctx, readerUID, claimUID)
			if (err == nil) != (failure == "") {
				t.Fatalf("graph result mismatch: %v", err)
			}
			if failure == "" && len(d.refs) != 10 {
				t.Fatalf("incomplete graph: %d", len(d.refs))
			}
			if failure == "" {
				storageKey["CRYPTO_KEY_VALUE"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 64)))
				ref, _ := d.ref("Secret", "storage-key", d.i.namespace)
				if _, err = d.owned(ctx, ref); err == nil {
					t.Fatal("same-UID key change adopted after graph verification")
				}
			}
			if _, present := a.objects[readerKey]; !present {
				t.Fatal("graph inspection removed reader")
			}
		})
	}
}
