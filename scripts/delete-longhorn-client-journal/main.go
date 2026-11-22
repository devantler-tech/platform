// Command delete-longhorn-client-journal removes the journal ConfigMap that the
// retired Longhorn client-maintenance tool left in production (#6472). Flux never
// managed that object, so no manifest change can remove it.
//
// It runs only through prod-operation.yaml. It deletes nothing while the retired
// tool is still part of the dispatched revision, and nothing unless the live
// object is exactly the tool's own completed journal. The delete is pinned to the
// UID and resourceVersion it read, so a replaced or changed object is refused by
// the API server instead of being removed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"time"
)

const (
	operation   = "delete-longhorn-client-journal"
	namespace   = "flux-system"
	name        = "longhorn-client-maintenance"
	holderKey   = "platform.devantler.tech/longhorn-client-maintenance"
	journalKey  = "journal.json"
	retiredTool = "scripts/maintain-longhorn-clients"
	rawPath     = "/api/v1/namespaces/" + namespace + "/configmaps/" + name
	maxResponse = 4 << 20
)

// runner runs one kubectl command with optional stdin and returns its stdout.
type runner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

type configMap struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
		OwnerReferences []json.RawMessage `json:"ownerReferences"`
		Finalizers      []string          `json:"finalizers"`
	} `json:"metadata"`
	Data       map[string]string `json:"data"`
	BinaryData map[string]string `json:"binaryData"`
}

func allowed(env func(string) string) bool {
	return env("GITHUB_ACTIONS") == "true" && env("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_REF") == "refs/heads/main" &&
		env("GITHUB_RUN_ATTEMPT") == "1" && env("GITHUB_ACTOR") == "devantler" &&
		env("RUNNER_ENVIRONMENT") == "github-hosted" && env("PROD_OPERATION") == operation &&
		regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(env("GITHUB_SHA"))
}

// read returns nil, nil when the journal does not exist.
func read(ctx context.Context, run runner) (*configMap, error) {
	out, err := run(ctx, nil, "--context=admin@prod", "--request-timeout=20s", "get", "configmap", name,
		"--namespace="+namespace, "--ignore-not-found", "--output=json")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var cm configMap
	if err = json.Unmarshal(out, &cm); err != nil {
		return nil, errors.New("journal read is not a JSON object")
	}
	return &cm, nil
}

// check accepts only the retired tool's own, completed, unmanaged journal.
func check(cm *configMap) error {
	m := cm.Metadata
	switch {
	case cm.Kind != "ConfigMap" || m.Name != name || m.Namespace != namespace:
		return errors.New("object is not the journal")
	case m.UID == "" || m.ResourceVersion == "":
		return errors.New("journal identity is unreadable")
	case len(m.OwnerReferences) != 0 || len(m.Labels) != 0:
		return errors.New("journal is owned or labelled by something else")
	case len(m.Finalizers) != 0:
		return errors.New("journal carries finalizers")
	case len(cm.Data) != 1 || len(cm.BinaryData) != 0:
		return errors.New("journal holds more than its record")
	}
	raw, ok := cm.Data[journalKey]
	if !ok {
		return errors.New("journal record is missing")
	}
	var j struct{ Holder, Phase string }
	if json.Unmarshal([]byte(raw), &j) != nil {
		return errors.New("journal record is malformed")
	}
	if j.Phase != "complete" {
		return errors.New("journal is not complete")
	}
	if j.Holder == "" || m.Annotations[holderKey] != j.Holder {
		return errors.New("journal holder does not match its annotation")
	}
	return nil
}

func deleteOptions(cm *configMap) ([]byte, error) {
	return json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "DeleteOptions",
		"preconditions": map[string]string{
			"uid":             cm.Metadata.UID,
			"resourceVersion": cm.Metadata.ResourceVersion,
		},
	})
}

func execute(ctx context.Context, env func(string) string, stat func(string) error, run runner, out io.Writer) int {
	report := func(code int, verdict, reason string) int {
		if reason != "" {
			_, _ = fmt.Fprintf(out, "LONGHORN_CLIENT_JOURNAL=%s reason=%q\n", verdict, reason)
		} else {
			_, _ = fmt.Fprintf(out, "LONGHORN_CLIENT_JOURNAL=%s\n", verdict)
		}
		return code
	}
	if !allowed(env) {
		return report(3, "UNKNOWN", "not the reviewed production dispatch")
	}
	// The dispatched revision is the current tip of main; until the retired tool
	// is gone from it, a failed deploy can still heal into code that reads the journal.
	if err := stat(retiredTool); err == nil {
		return report(1, "HOLD", "the retired tool is still on main")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return report(3, "UNKNOWN", "cannot tell whether the retired tool is gone")
	}
	cm, err := read(ctx, run)
	if err != nil {
		return report(3, "UNKNOWN", "journal read failed")
	}
	if cm == nil {
		return report(0, "ABSENT", "")
	}
	if err = check(cm); err != nil {
		return report(1, "HOLD", err.Error())
	}
	body, err := deleteOptions(cm)
	if err != nil {
		return report(3, "UNKNOWN", "delete request could not be built")
	}
	// A precondition the server rejects (replaced or changed journal) fails here.
	_, deleteErr := run(ctx, body, "--context=admin@prod", "--request-timeout=20s", "delete", "--raw", rawPath, "--filename=-")
	after, err := read(ctx, run)
	switch {
	case err != nil:
		return report(3, "UNKNOWN", "journal read after delete failed")
	case after == nil:
		return report(0, "DELETED", "")
	case after.Metadata.UID != cm.Metadata.UID:
		return report(1, "HOLD", "a different journal replaced the one that was read")
	case deleteErr != nil:
		return report(1, "HOLD", "the API server refused the pinned delete")
	default:
		return report(3, "UNKNOWN", "journal still present after delete")
	}
}

type boundedOutput struct {
	body     []byte
	overflow bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > maxResponse-len(b.body) {
		b.overflow = true
		return 0, errors.New("response limit")
	}
	b.body = append(b.body, data...)
	return len(data), nil
}

func kubectl(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "kubectl", args...)
	command.Stdin = bytes.NewReader(stdin)
	var stdout boundedOutput
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || stdout.overflow {
		return nil, errors.New("kubectl call failed")
	}
	return stdout.body, nil
}

func main() {
	if len(os.Args) != 1 {
		fmt.Println("LONGHORN_CLIENT_JOURNAL=UNKNOWN")
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	stat := func(path string) error { _, err := os.Stat(path); return err }
	code := execute(ctx, os.Getenv, stat, kubectl, os.Stdout)
	cancel()
	os.Exit(code)
}
