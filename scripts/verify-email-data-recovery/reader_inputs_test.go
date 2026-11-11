package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The independent witnesses identify the existing copy; labels alone must
// never select which credentials are saved for a later restart.
func readerInputFixture(t *testing.T) ([]object, string, string) {
	t.Helper()
	graph := append(recoveryResources(retainedRecovery, strings.Repeat("a", 64), "restore:"+strings.Repeat("b", 64)), recoveryReader(retainedRecovery, fixtureImage))
	var readerUID, claimUID string
	for index, o := range graph {
		m := child(o, "metadata")
		m["uid"] = fmt.Sprintf("%08x-1111-2222-3333-444444444444", index+1)
		m["resourceVersion"] = fmt.Sprint(index + 100)
		if text(o, "kind") == "Secret" {
			data := object{}
			for k, v := range child(o, "stringData") {
				data[k] = base64.StdEncoding.EncodeToString([]byte(v.(string)))
			}
			o["data"] = data
			delete(o, "stringData")
		}
		if text(o, "kind") == "Pod" {
			readerUID = text(m, "uid")
			o["status"] = object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}
		}
		if text(o, "kind") == "PersistentVolumeClaim" {
			claimUID = text(m, "uid")
			child(o, "spec")["volumeName"] = "pvc-existing-recovery"
			o["status"] = object{"phase": "Bound"}
		}
	}
	return graph, readerUID, claimUID
}

func TestReaderInputBundleKeepsTheActualPerCopyCredentials(t *testing.T) {
	graph, reader, claim := readerInputFixture(t)
	bundle, err := captureRetainedReaderInputs(retainedRecovery, fixtureImage, reader, claim, "ffffffff-1111-2222-3333-444444444444", graph)
	if err != nil {
		t.Fatalf("independently bound retained inputs refused: %v", err)
	}
	if bundle.ReaderUID != reader || bundle.ClaimUID != claim || len(bundle.Inputs) != 8 {
		t.Fatal("input custody lost the independent reader or complete restart inputs")
	}
	secrets := 0
	for _, input := range bundle.Inputs {
		if len(input.Digest) != 64 || input.UID == "" {
			t.Fatal("restart input lacks a frozen identity/content binding")
		}
		if input.Kind == "Secret" {
			secrets++
			if !reflect.DeepEqual(child(input.Values, "data"), child(graph[map[string]int{"storage-key": 3, "test-authentication": 4}[input.Name]], "data")) {
				t.Fatal("operational credentials substituted for this copy's inputs")
			}
		}
	}
	if secrets != 2 {
		t.Fatal("both retained encryption and recovery authentication are required")
	}
	graph[0], graph[7] = graph[7], graph[0]
	reordered, err := captureRetainedReaderInputs(retainedRecovery, fixtureImage, reader, claim, "ffffffff-1111-2222-3333-444444444444", graph)
	if err != nil || !reflect.DeepEqual(bundle, reordered) {
		t.Fatal("discovery order changed the retained-input identity")
	}
	before, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	child(graph[3], "data")["CRYPTO_KEY_VALUE"] = "changed-after-capture"
	after, err := json.Marshal(bundle)
	if err != nil || string(before) != string(after) {
		t.Fatal("later live mutations rewrote the captured custody inputs")
	}
}

func TestReaderInputBundleBindsValidChangedKeyBytes(t *testing.T) {
	graph, reader, claim := readerInputFixture(t)
	source := "ffffffff-1111-2222-3333-444444444444"
	before, err := captureRetainedReaderInputs(retainedRecovery, fixtureImage, reader, claim, source, graph)
	if err != nil {
		t.Fatal(err)
	}
	child(graph[3], "data")["CRYPTO_KEY_VALUE"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 64)))
	after, err := captureRetainedReaderInputs(retainedRecovery, fixtureImage, reader, claim, source, graph)
	if err != nil {
		t.Fatal(err)
	}
	if before.Inputs[3].Digest == after.Inputs[3].Digest {
		t.Fatal("a different valid encryption key inherited the original content binding")
	}
}

func TestReaderInputBundleRefusesBorrowedReplacedOrIncompleteInputs(t *testing.T) {
	for _, failure := range []string{"reader", "claim", "source", "missing", "duplicate", "key", "authentication", "configuration", "storage-class", "executable", "not-ready", "terminating", "controller", "namespace", "identity", "resource-version", "labels", "foreign-target"} {
		t.Run(failure, func(t *testing.T) {
			graph, reader, claim := readerInputFixture(t)
			source := "ffffffff-1111-2222-3333-444444444444"
			target := retainedRecovery
			switch failure {
			case "reader":
				reader = "eeeeeeee-1111-2222-3333-444444444444"
			case "claim":
				claim = "eeeeeeee-1111-2222-3333-444444444444"
			case "source":
				source = claim
			case "missing":
				graph = graph[:len(graph)-1]
			case "duplicate":
				graph = append(graph, graph[0])
			case "key":
				child(graph[3], "data")["CRYPTO_KEY_VALUE"] = "not-base64"
			case "authentication":
				child(graph[4], "data")["credential"] = base64.StdEncoding.EncodeToString([]byte("another-account"))
			case "configuration":
				child(graph[5], "data")["config.json"] = `{}`
			case "storage-class":
				child(graph[2], "parameters")["csi.storage.k8s.io/node-stage-secret-namespace"] = "private-mail"
			case "executable":
				items(child(graph[7], "spec"), "containers")[0]["command"] = []any{"unreviewed"}
			case "not-ready":
				graph[7]["status"] = object{"phase": "Pending"}
			case "terminating":
				child(graph[3], "metadata")["deletionTimestamp"] = "2026-01-01T00:00:00Z"
			case "controller":
				child(graph[7], "metadata")["ownerReferences"] = []any{object{"controller": true}}
			case "namespace":
				child(graph[3], "metadata")["namespace"] = "private-mail"
			case "identity":
				child(graph[4], "metadata")["uid"] = child(graph[3], "metadata")["uid"]
			case "resource-version":
				delete(child(graph[3], "metadata"), "resourceVersion")
			case "labels":
				child(child(graph[3], "metadata"), "labels")[recoveryLabel] = "another-run"
			case "foreign-target":
				target.run = "12345678"
			}
			if _, err := captureRetainedReaderInputs(target, fixtureImage, reader, claim, source, graph); err == nil {
				t.Fatal("unqualified input bundle could enter recovery custody")
			}
		})
	}
}
