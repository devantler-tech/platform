package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeKubernetes struct {
	calls  int
	invoke func([]byte, []string) ([]byte, error)
}

func (k *fakeKubernetes) call(_ context.Context, input []byte, args ...string) ([]byte, error) {
	k.calls++
	return k.invoke(input, args)
}
func TestUnknownCreationStopsCleanupWithoutGuessingOwnership(t *testing.T) {
	k := &fakeKubernetes{invoke: func([]byte, []string) ([]byte, error) { return nil, errors.New("ambiguous private API response") }}
	r := runner{k: k, run: "123", namespace: "email-storage-canary-123", refs: map[string]reference{}}
	if _, err := r.create(context.Background(), namespaceManifest(r.namespace, r.run)); err == nil {
		t.Fatal("unknown create treated as failure without a possible side effect")
	}
	if r.cleanup(context.Background()) == nil || k.calls != 1 {
		t.Fatal("cleanup guessed ownership after an uncertain create")
	}
}
func TestCleanupRefusesReplacementAndUntrackedUID(t *testing.T) {
	old := reference{kind: "Secret", name: "encryption-key", namespace: "email-storage-canary-123", uid: "original-uid"}
	k := &fakeKubernetes{invoke: func(_ []byte, args []string) ([]byte, error) {
		if args[0] != "get" {
			t.Fatal("cleanup mutated a replacement resource")
		}
		return json.Marshal(object{"metadata": object{"name": old.name, "namespace": old.namespace, "uid": "new-uid", "labels": object{labelKey: "123"}}})
	}}
	r := runner{k: k, run: "123", refs: map[string]reference{old.key(): old}}
	forged := old
	forged.uid = "new-uid"
	if r.remove(context.Background(), forged) == nil || k.calls != 0 {
		t.Fatal("cleanup accepted an unrecorded UID")
	}
	if r.remove(context.Background(), old) == nil || k.calls != 1 {
		t.Fatal("cleanup accepted a resource replacement")
	}
}
func TestDeleteUsesServerPreconditionAndWaitsForAbsence(t *testing.T) {
	ref := reference{kind: "Pod", name: "probe-read", namespace: "email-storage-canary-123", uid: "original-uid"}
	deleted := false
	k := &fakeKubernetes{invoke: func(input []byte, args []string) ([]byte, error) {
		if args[0] == "get" {
			if deleted {
				return nil, nil
			}
			return json.Marshal(object{"metadata": object{"uid": ref.uid, "labels": object{labelKey: "123"}}})
		}
		if args[0] != "delete" || args[1] != "--raw=/api/v1/namespaces/email-storage-canary-123/pods/probe-read" || args[2] != "-f=-" {
			t.Fatal("unexpected deletion transport")
		}
		var body object
		if json.Unmarshal(input, &body) != nil || text(child(body, "preconditions"), "uid") != ref.uid {
			t.Fatal("server deletion lacks exact UID")
		}
		deleted = true
		return []byte(`{}`), nil
	}}
	r := runner{k: k, run: "123", refs: map[string]reference{ref.key(): ref}}
	if err := r.remove(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if !deleted || len(r.refs) != 0 || k.calls != 3 {
		t.Fatal("deletion receipt substituted for observed absence")
	}
}
func TestCleanupRetainsIdentityAfterUnknownDelete(t *testing.T) {
	ref := reference{kind: "Pod", name: "probe-read", namespace: "email-storage-canary-123", uid: "original-uid"}
	k := &fakeKubernetes{invoke: func(_ []byte, args []string) ([]byte, error) {
		if args[0] == "get" {
			return json.Marshal(object{"metadata": object{"uid": ref.uid, "labels": object{labelKey: "123"}}})
		}
		return nil, errors.New("private deletion response")
	}}
	r := runner{k: k, run: "123", refs: map[string]reference{ref.key(): ref}}
	if err := r.remove(context.Background(), ref); err == nil || len(r.refs) != 1 {
		t.Fatal("uncertain deletion erased the recorded identity")
	}
}
func TestCleanupPathCannotReachCompanyMail(t *testing.T) {
	for _, ref := range []reference{{kind: "Secret", name: "email-storage-key", namespace: "private-mail"}, {kind: "Namespace", name: "private-mail"}, {kind: "StorageClass", name: "email-encrypted"}, {kind: "Pod", name: "../private-mail", namespace: "email-storage-canary-123"}} {
		if path, err := ref.path(); err == nil {
			t.Fatal("operational cleanup path accepted:", path)
		}
	}
}
func TestMalformedStatusCannotProveKeyRefusal(t *testing.T) {
	for _, status := range []any{"unknown", []any{"unknown"}, object{"probe": "waiting"}} {
		pod := pendingPod()
		child(pod, "status")["containerStatuses"] = status
		if negativeMount(pod, []object{mountEvent("volume \"pvc-test\": No key available with this passphrase.")}, "pod-uid", "pvc-test", "wrong") {
			t.Fatal("malformed pod status accepted")
		}
	}
}
func TestNoClusterMutationWhenVersionReadFails(t *testing.T) {
	k := &fakeKubernetes{invoke: func(_ []byte, args []string) ([]byte, error) {
		if args[0] != "get" {
			t.Fatal("mutation preceded version verification")
		}
		return nil, errors.New(strings.Repeat("sensitive", 10))
	}}
	r := runner{k: k, run: "123", namespace: "email-storage-canary-123", refs: map[string]reference{}}
	if r.execute(context.Background()) == nil || k.calls != 1 || len(r.refs) != 0 {
		t.Fatal("version failure did not stop the canary")
	}
}
