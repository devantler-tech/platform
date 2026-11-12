package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readerCustodyDispatch supplies the fixed first-attempt workflow identity for custody tests.
func readerCustodyDispatch() map[string]string {
	env := confirmedInvocation()
	env["GITHUB_ACTIONS"] = "true"
	env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/prepare-retained-email-reader-inputs.yaml@refs/heads/main"
	env["EMAIL_READER_INPUTS_CONFIRM"] = "prepare-retained-email-reader-inputs"
	return env
}

// readerCustodyNativeFixture separates retained inputs from a distinct serving mailbox graph.
func readerCustodyNativeFixture(t *testing.T) (*cleanupAPI, string, string) {
	t.Helper()
	t.Chdir("../..")
	graph, reader, claim := readerInputFixture(t)
	image, err := loadRecoveryImage()
	if err != nil {
		t.Fatal(err)
	}
	items(child(graph[7], "spec"), "containers")[0]["image"] = image
	a := &cleanupAPI{objects: map[string]object{}}
	for _, o := range graph {
		m := child(o, "metadata")
		a.objects[cleanupKey(text(o, "kind"), text(m, "namespace"), text(m, "name"))] = o
	}
	pod, pvc, pv := servingFixture()
	items(child(pod, "spec"), "containers")[0]["image"] = image
	child(pvc, "metadata")["uid"] = "ffffffff-1111-2222-3333-444444444444"
	child(pvc, "spec")["volumeName"] = "pvc-serving-volume"
	child(pv, "metadata")["name"] = "pvc-serving-volume"
	child(child(pv, "spec"), "claimRef")["uid"] = child(pvc, "metadata")["uid"]
	child(child(pv, "spec"), "csi")["volumeHandle"] = "pvc-serving-volume"
	a.objects[cleanupKey("Pod", "private-mail", "private-mail-0")] = pod
	a.objects[cleanupKey("PersistentVolumeClaim", "private-mail", "private-mail-data")] = pvc
	a.objects[cleanupKey("PersistentVolume", "", "pvc-serving-volume")] = pv
	return a, reader, claim
}

// TestReaderCustodyDispatchRefusesBeforeAnyAccess proves invalid dispatch never reads credentials or invokes native clients.
func TestReaderCustodyDispatchRefusesBeforeAnyAccess(t *testing.T) {
	for _, invalid := range []string{"GITHUB_ACTIONS", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "GITHUB_ACTOR", "GITHUB_WORKFLOW_REF", "RUNNER_ENVIRONMENT", "GITHUB_SHA", "GITHUB_RUN_ID", "EMAIL_READER_INPUTS_CONFIRM", "EMAIL_RETAINED_READER_UID", "EMAIL_RETAINED_CLAIM_UID"} {
		t.Run(invalid, func(t *testing.T) {
			env := readerCustodyDispatch()
			env["EMAIL_RETAINED_READER_UID"] = "11111111-1111-2222-3333-444444444444"
			env["EMAIL_RETAINED_CLAIM_UID"] = "22222222-1111-2222-3333-444444444444"
			env[invalid] = "invalid"
			k := &refusedAPI{}
			callbacks := 0
			var out bytes.Buffer
			code := runReaderInputCustody(context.Background(), func(key string) string { return env[key] }, k,
				func(context.Context, string) error { callbacks++; return nil },
				func(func(string) string) (*readerCustodyStore, error) { callbacks++; return nil, nil }, &out)
			if code != 2 || k.calls != 0 || callbacks != 0 || !strings.Contains(out.String(), "stage=dispatch") {
				t.Fatal("foreign dispatch reached credentials, native resources or source execution")
			}
		})
	}
}

// TestReaderCustodyOperatorStoresInputsWithoutChangingAnyKubernetesObject checks storage readback and rejects input changes around the write.
func TestReaderCustodyOperatorStoresInputsWithoutChangingAnyKubernetesObject(t *testing.T) {
	for _, changeAt := range []int{0, 2, 3} {
		t.Run(map[int]string{0: "stable", 2: "changed-before-write", 3: "changed-after-write"}[changeAt], func(t *testing.T) {
			a, reader, claim := readerCustodyNativeFixture(t)
			before, err := json.Marshal(a.objects)
			if err != nil {
				t.Fatal(err)
			}
			posts, sourceChecks := 0, 0
			var saved object
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sys/mounts" {
					_, _ = w.Write([]byte(`{"data":{"secret/":{"type":"kv","options":{"version":"2"}}}}`))
					return
				}
				if r.Method == http.MethodPost {
					posts++
					var payload object
					if json.NewDecoder(r.Body).Decode(&payload) != nil || child(payload, "options")["cas"] != float64(0) {
						t.Error("operator escaped create-only custody")
					}
					saved = child(payload, "data")
					_, _ = w.Write([]byte(`{"data":{"version":1}}`))
					return
				}
				if r.URL.Path == retainedReaderDataPath && saved != nil {
					custodyResponse(t, w, saved, 1)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			env := readerCustodyDispatch()
			env["EMAIL_RETAINED_READER_UID"], env["EMAIL_RETAINED_CLAIM_UID"] = reader, claim
			var out bytes.Buffer
			code := runReaderInputCustody(context.Background(), func(key string) string { return env[key] }, a,
				func(context.Context, string) error {
					sourceChecks++
					if changeAt != 0 && sourceChecks == changeAt {
						child(a.objects[cleanupKey("ConfigMap", retainedRecovery.namespace, "datastore")], "data")["config.json"] = `{}`
					}
					return nil
				}, func(func(string) string) (*readerCustodyStore, error) {
					return newReaderCustodyStore(server.URL, "fixture-reader-custody-token")
				}, &out)
			if changeAt != 0 {
				expectedPosts := 0
				if changeAt == 3 {
					expectedPosts = 1
				}
				if code != 2 || posts != expectedPosts || strings.Contains(out.String(), "CUSTODY=STORED") {
					t.Fatal("changed native inputs produced a write or a successful custody receipt")
				}
				return
			}
			after, err := json.Marshal(a.objects)
			if code != 0 || posts != 1 || sourceChecks != 3 || err != nil || !bytes.Equal(before, after) {
				t.Fatalf("operator failed its read-only native boundary: code=%d posts=%d checks=%d output=%q", code, posts, sourceChecks, out.String())
			}
			if !strings.Contains(out.String(), "restart=NOT_AUTHORIZED recovery=UNVERIFIED") || strings.Contains(out.String(), reader) || strings.Contains(out.String(), "CRYPTO_KEY_VALUE") || strings.Contains(out.String(), "fixture-reader-custody-token") {
				t.Fatal("custody receipt exposed inputs or granted lifecycle/recovery authority")
			}
		})
	}
}

// TestReaderCustodyKubernetesCapabilityHasNoMutationOrForeignSecretRead verifies rejected requests never reach the underlying client.
func TestReaderCustodyKubernetesCapabilityHasNoMutationOrForeignSecretRead(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "Pod", "reader"}, {"create", "-f=-"}, {"patch", "Secret", "storage-key"},
		{"get", "Secret", "openbao-unseal", "--ignore-not-found=true", "-o=json", "--namespace=openbao"},
		{"get", "Pod", "reader", "--ignore-not-found=true", "-o=json", "--namespace=private-mail"},
		{"get", "Secret", "storage-key", "--ignore-not-found=true", "-o=json", "--namespace=" + retainedRecovery.namespace, "--raw=/"},
	} {
		k := &refusedAPI{}
		_, err := (readerInputKubernetes{inner: k}).call(context.Background(), nil, args...)
		if err == nil || k.calls != 0 {
			t.Fatal("input custody capability reached a mutation or foreign resource")
		}
	}
	if _, err := (readerInputKubernetes{}).call(context.Background(), nil); !errors.Is(err, errReaderInputRead) {
		t.Fatal("missing native reader did not fail closed")
	}
}

// TestReaderCustodyLostOutputOrUnverifiedSourceStopsBeforeNativeAccess prevents silent operation without source and output evidence.
func TestReaderCustodyLostOutputOrUnverifiedSourceStopsBeforeNativeAccess(t *testing.T) {
	for _, failure := range []string{"output", "source"} {
		t.Run(failure, func(t *testing.T) {
			env := readerCustodyDispatch()
			env["EMAIL_RETAINED_READER_UID"] = "11111111-1111-2222-3333-444444444444"
			env["EMAIL_RETAINED_CLAIM_UID"] = "22222222-1111-2222-3333-444444444444"
			k := &refusedAPI{}
			var out bytes.Buffer
			writer := &observationFailingWriter{base: &out, remaining: 10}
			if failure == "output" {
				writer.remaining = 0
			}
			loaded := 0
			code := runReaderInputCustody(context.Background(), func(key string) string { return env[key] }, k,
				func(context.Context, string) error { return errors.New("unverified current main") },
				func(func(string) string) (*readerCustodyStore, error) { loaded++; return nil, nil }, writer)
			if code != 2 || k.calls != 0 || loaded != 0 || strings.Contains(out.String(), "CUSTODY=STORED") {
				t.Fatal("lost output or source proof reached native credentials")
			}
		})
	}
}

// TestReaderCustodyWrongIndependentReaderStopsBeforeKeyAccess checks identity before any retained or service credential is read.
func TestReaderCustodyWrongIndependentReaderStopsBeforeKeyAccess(t *testing.T) {
	a, _, claim := readerCustodyNativeFixture(t)
	secretReads, loaded := 0, 0
	a.onGet = func(key string) {
		if strings.HasPrefix(key, "Secret/") {
			secretReads++
		}
	}
	env := readerCustodyDispatch()
	env["EMAIL_RETAINED_READER_UID"] = "eeeeeeee-1111-2222-3333-444444444444"
	env["EMAIL_RETAINED_CLAIM_UID"] = claim
	var out bytes.Buffer
	code := runReaderInputCustody(context.Background(), func(key string) string { return env[key] }, a,
		func(context.Context, string) error { return nil },
		func(func(string) string) (*readerCustodyStore, error) { loaded++; return nil, nil }, &out)
	if code != 2 || secretReads != 0 || loaded != 0 || !strings.Contains(out.String(), "stage=inputs") {
		t.Fatal("a replaced reader reached encryption or store credentials")
	}
}
