package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLargeJournalPatchUsesStdinAndPreservesCAS(t *testing.T) {
	o := resource("ConfigMap", journalNS, journalName)
	payload := strings.Repeat("retained-evidence-", 32000)
	patch := append(tests(o), object{"op": "replace", "path": "/data/journal.json", "value": payload})
	client := client{command: func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		if len(input) < 128*1024 {
			t.Fatal("large patch body not delivered on standard input")
		}
		for _, arg := range args {
			if len(arg) > 1024 || arg == "--patch" {
				t.Fatal("journal transported through command arguments")
			}
		}
		expected := []string{"patch", "configmap", journalName, "-n", journalNS, "--type=json", "--field-manager=platform-longhorn-client-maintenance", "--patch-file=/dev/stdin", "-o", "name"}
		if !reflect.DeepEqual(args, expected) {
			t.Fatalf("command target changed: %v", args)
		}
		var received []object
		if json.Unmarshal(input, &received) != nil || !reflect.DeepEqual(received, patch) {
			t.Fatal("stdin patch or UID/resourceVersion preconditions changed")
		}
		return []byte("configmap/" + journalName), nil
	}}
	if err := client.patch(context.Background(), "configmap", o, patch); err != nil {
		t.Fatal(err)
	}
}

// Exercise the actual kubectl patch-file reader, with --local and an absent
// kubeconfig. No API request, live resource, or production credential is used.
func TestNativeKubectlConsumesLargePatchAndRejectsWrongUID(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "object.json")
	o := resource("ConfigMap", journalNS, journalName)
	o["data"] = object{"journal.json": "before"}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("retained-evidence-", 32000)
	patch := append(tests(o), object{"op": "replace", "path": "/data/journal.json", "value": payload})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := client{command: func(c context.Context, input []byte, args ...string) ([]byte, error) {
		local := append([]string{"patch", "--local", "-f", file}, args[3:]...)
		// Keep the production command's patch transport, only changing its
		// resource source and output format to observe the resulting local object.
		local[len(local)-1] = "json"
		cmd := exec.CommandContext(c, "kubectl", local...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(tmp, "absent"))
		cmd.Stdin = bytes.NewReader(input)
		return cmd.Output()
	}}
	// Invoke the same client.patch production path and capture its native result.
	var native []byte
	execute := client.command
	client.command = func(c context.Context, input []byte, args ...string) ([]byte, error) {
		data, err := execute(c, input, args...)
		native = data
		return data, err
	}
	if err := client.patch(ctx, "configmap", o, patch); err != nil {
		t.Fatalf("native stdin patch failed: %v", err)
	}
	var after object
	if json.Unmarshal(native, &after) != nil || str(after, "data", "journal.json") != payload {
		t.Fatal("native patch truncated or changed journal")
	}
	wrong := resource("ConfigMap", journalNS, journalName)
	wrong["metadata"].(object)["uid"] = "wrong-uid"
	if err := client.patch(ctx, "configmap", wrong, append(tests(wrong), patch[len(patch)-1])); err == nil {
		t.Fatal("native UID test silently lost")
	}
}

func TestStdinPatchKeepsConflictClassification(t *testing.T) {
	client := client{command: func(context.Context, []byte, ...string) ([]byte, error) {
		return nil, errors.New("server rejected request (Conflict)")
	}}
	if err := client.patch(context.Background(), "configmap", resource("ConfigMap", journalNS, journalName), nil); !errors.Is(err, errPatchConflict) {
		t.Fatalf("conflict classification lost: %v", err)
	}
}
