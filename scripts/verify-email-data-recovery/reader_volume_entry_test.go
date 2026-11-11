package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

type readerVolumeAPI struct {
	inner                   *cleanupAPI
	name                    string
	patches                 int
	beforePatch, afterPatch func()
	lostResponse            bool
}

func (a *readerVolumeAPI) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if args[0] != "patch" {
		if len(args) > 1 && args[1] == "Secret" {
			return nil, errors.New("secret-read-forbidden")
		}
		return a.inner.call(ctx, input, args...)
	}
	a.patches++
	if !reflect.DeepEqual(args, []string{"patch", "PersistentVolume", a.name, "--type=json", "--patch-file=/dev/stdin", "-o=json"}) {
		return nil, errors.New("patch-capability")
	}
	if a.beforePatch != nil {
		a.beforePatch()
	}
	key := cleanupKey("PersistentVolume", "", a.name)
	raw, _ := json.Marshal(a.inner.objects[key])
	patch, err := jsonpatch.DecodePatch(input)
	if err != nil {
		return nil, err
	}
	updated, err := patch.Apply(raw)
	if err != nil {
		return nil, err
	}
	var o object
	if json.Unmarshal(updated, &o) != nil {
		return nil, errors.New("patch-result")
	}
	child(o, "metadata")["resourceVersion"] = "501"
	a.inner.objects[key] = o
	if a.afterPatch != nil {
		a.afterPatch()
	}
	if a.lostResponse {
		return nil, errors.New("uncertain-patch-response")
	}
	return json.Marshal(a.inner.objects[key])
}

func readerVolumeDispatch() map[string]string {
	env := readerCustodyDispatch()
	delete(env, "EMAIL_READER_INPUTS_CONFIRM")
	env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/protect-retained-email-volume.yaml@refs/heads/main"
	env["EMAIL_RETAINED_VOLUME_CONFIRM"] = "protect-retained-email-volume"
	return env
}

func readerVolumeNativeFixture(t *testing.T) (*readerVolumeAPI, map[string]string) {
	t.Helper()
	a, _, _ := readerCustodyNativeFixture(t)
	reader, claim, pv, _, witness := retainedVolumeFixture(t)
	image, err := loadRecoveryImage()
	if err != nil {
		t.Fatal(err)
	}
	items(child(reader, "spec"), "containers")[0]["image"] = image
	for _, o := range []object{reader, claim, pv} {
		m := child(o, "metadata")
		a.objects[cleanupKey(text(o, "kind"), text(m, "namespace"), text(m, "name"))] = o
	}
	env := readerVolumeDispatch()
	env["EMAIL_RETAINED_READER_UID"], env["EMAIL_RETAINED_CLAIM_UID"], env["EMAIL_RETAINED_PV_UID"] = witness.ReaderUID, witness.ClaimUID, witness.PVUID
	return &readerVolumeAPI{inner: a, name: text(child(pv, "metadata"), "name")}, env
}

func TestRetainedVolumeProtectionChangesOnlyTheBoundReclaimPolicyAndIsIdempotent(t *testing.T) {
	a, env := readerVolumeNativeFixture(t)
	before, _ := json.Marshal(a.inner.objects)
	for pass := 0; pass < 2; pass++ {
		var out bytes.Buffer
		checks := 0
		code := runRetainedVolumeProtection(context.Background(), func(k string) string { return env[k] }, a, func(context.Context, string) error { checks++; return nil }, &out)
		if code != 0 || a.patches != 1 || checks < 2 || !strings.Contains(out.String(), "EMAIL_RETAINED_VOLUME=PROTECTED") || !strings.Contains(out.String(), "reader=RETAINED restart=NOT_AUTHORIZED") {
			t.Fatalf("fixed metadata protection was not verified: code=%d patches=%d checks=%d output=%q", code, a.patches, checks, out.String())
		}
		if strings.Contains(out.String(), env["EMAIL_RETAINED_PV_UID"]) || strings.Contains(out.String(), a.name) {
			t.Fatal("private identity reached output")
		}
	}
	var original map[string]object
	if json.Unmarshal(before, &original) != nil {
		t.Fatal("fixture")
	}
	key := cleanupKey("PersistentVolume", "", a.name)
	child(original[key], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	child(original[key], "metadata")["resourceVersion"] = "501"
	if !reflect.DeepEqual(original, a.inner.objects) {
		t.Fatal("protection changed data, readers, keys, configuration or another storage field")
	}
}

func TestRetainedVolumeProtectionRejectsForeignDispatchBeforeAccess(t *testing.T) {
	for _, field := range []string{"GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_ACTOR", "GITHUB_RUN_ATTEMPT", "RUNNER_ENVIRONMENT", "GITHUB_WORKFLOW_REF", "GITHUB_RUN_ID", "GITHUB_SHA", "EMAIL_RETAINED_VOLUME_CONFIRM", "EMAIL_RETAINED_READER_UID", "EMAIL_RETAINED_CLAIM_UID", "EMAIL_RETAINED_PV_UID"} {
		t.Run(field, func(t *testing.T) {
			a, env := readerVolumeNativeFixture(t)
			env[field] = "foreign"
			calls := 0
			a.inner.onGet = func(string) { calls++ }
			var out bytes.Buffer
			code := runRetainedVolumeProtection(context.Background(), func(k string) string { return env[k] }, a, func(context.Context, string) error { calls++; return nil }, &out)
			if code != 2 || calls != 0 || a.patches != 0 || !strings.Contains(out.String(), "stage=dispatch") {
				t.Fatal("foreign dispatch reached native state or source execution")
			}
		})
	}
}

func TestRetainedVolumeProtectionNeverRetriesOrCertifiesAnUncertainMutation(t *testing.T) {
	for _, failure := range []string{"stale-version", "replacement", "lost-response", "changed-readback", "changed-before-write", "changed-after-write"} {
		t.Run(failure, func(t *testing.T) {
			a, env := readerVolumeNativeFixture(t)
			key := cleanupKey("PersistentVolume", "", a.name)
			switch failure {
			case "stale-version":
				a.beforePatch = func() { child(a.inner.objects[key], "metadata")["resourceVersion"] = "502" }
			case "replacement":
				a.beforePatch = func() { child(a.inner.objects[key], "metadata")["uid"] = "replaced" }
			case "lost-response":
				a.lostResponse = true
			case "changed-readback":
				a.afterPatch = func() { child(child(a.inner.objects[key], "spec"), "csi")["volumeHandle"] = "foreign" }
			}
			checks := 0
			var out bytes.Buffer
			code := runRetainedVolumeProtection(context.Background(), func(k string) string { return env[k] }, a, func(context.Context, string) error {
				checks++
				if failure == "changed-before-write" && checks == 2 {
					child(a.inner.objects[cleanupKey("PersistentVolumeClaim", retainedRecovery.namespace, "data")], "metadata")["uid"] = "changed"
				}
				if failure == "changed-after-write" && checks == 3 {
					return errors.New("source-changed")
				}
				return nil
			}, &out)
			wantPatches := 1
			if failure == "changed-before-write" {
				wantPatches = 0
			}
			if code != 2 || a.patches != wantPatches || strings.Contains(out.String(), "VOLUME=PROTECTED") {
				t.Fatalf("uncertain protection was retried or certified: code=%d patches=%d output=%q", code, a.patches, out.String())
			}
		})
	}
}

func TestRetainedVolumeCapabilityCannotBorrowCustodyOrLifecyclePermissions(t *testing.T) {
	for _, args := range [][]string{
		{"get", "Secret", "storage-key", "--ignore-not-found=true", "-o=json", "--namespace=" + retainedRecovery.namespace},
		{"delete", "Pod", "reader"}, {"create", "-f=-"},
		{"patch", "PersistentVolumeClaim", "data", "--type=json", "--patch-file=/dev/stdin", "-o=json"},
		{"patch", "PersistentVolume", "pvc-foreign", "--type=json", "--patch-file=/dev/stdin", "-o=json"},
		{"patch", "PersistentVolume", "pvc-bound", "--type=merge", "--patch-file=/dev/stdin", "-o=json"},
		{"patch", "PersistentVolume", "pvc-bound", "--type=json", "--patch-file=/dev/stdin", "-o=json", "--subresource=status"},
	} {
		inner := &refusedAPI{}
		native := &retainedVolumeKubernetes{inner: inner, name: "pvc-bound", patch: []byte("approved-patch")}
		if _, err := native.call(context.Background(), []byte("approved-patch"), args...); err == nil || inner.calls != 0 {
			t.Fatal("metadata protection borrowed secret, reader, claim or arbitrary patch authority")
		}
	}
	for _, input := range [][]byte{nil, []byte("foreign-patch")} {
		inner := &refusedAPI{}
		native := &retainedVolumeKubernetes{inner: inner, name: "pvc-bound", patch: []byte("approved-patch")}
		if _, err := native.call(context.Background(), input, "patch", "PersistentVolume", "pvc-bound", "--type=json", "--patch-file=/dev/stdin", "-o=json"); err == nil || inner.calls != 0 {
			t.Fatal("unqualified patch payload reached Kubernetes")
		}
	}
}

func TestRetainedVolumeCommandUsesItsSeparateProtectedDispatch(t *testing.T) {
	if os.Getenv("EMAIL_RETAINED_VOLUME_CHILD") == "1" {
		os.Args = []string{"verify-email-data-recovery", "--protect-retained-volume"}
		main()
		return
	}
	for _, confirmed := range []bool{false, true} {
		command := exec.Command(os.Args[0], "-test.run=^TestRetainedVolumeCommandUsesItsSeparateProtectedDispatch$")
		command.Env = []string{"EMAIL_RETAINED_VOLUME_CHILD=1", "PATH=/nonexistent", "GOCOVERDIR=" + t.TempDir()}
		stage := "dispatch"
		if confirmed {
			for key, value := range readerVolumeDispatch() {
				command.Env = append(command.Env, key+"="+value)
			}
			command.Env = append(command.Env, "EMAIL_RETAINED_READER_UID=11111111-1111-2222-3333-444444444444", "EMAIL_RETAINED_CLAIM_UID=22222222-1111-2222-3333-444444444444", "EMAIL_RETAINED_PV_UID=33333333-1111-2222-3333-444444444444")
			stage = "source"
		}
		output, err := command.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "EMAIL_RETAINED_VOLUME=UNVERIFIED stage="+stage+" reader=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED") || strings.Contains(string(output), "EMAIL_DATA_RECOVERY=PASS") {
			t.Fatalf("volume command lost its separate dispatch boundary: %q", output)
		}
	}
}
