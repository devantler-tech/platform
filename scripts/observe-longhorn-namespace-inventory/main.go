// Command observe-longhorn-namespace-inventory checks, read-only, whether
// production returns a complete metadata inventory of the Longhorn namespace.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"time"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
	"github.com/devantler-tech/platform/internal/strictjson"
)

type boundedOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (b *boundedOutput) Len() int      { return b.data.Len() }
func (b *boundedOutput) Bytes() []byte { return b.data.Bytes() }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.exceeded || len(p) > 2*1024*1024-b.Len() {
		b.exceeded = true
		b.data.Reset()
		return 0, errors.New("command response exceeded its bound")
	}
	return b.data.Write(p)
}

func active(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Now().Before(deadline)
}

func fixedCommand(ctx context.Context, program string, args ...string) ([]byte, error) {
	allowed := program == "bash" && len(args) == 2 && args[0] == "./scripts/verify-prod-recovery-source.sh" && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(args[1])
	allowed = allowed || program == "kubectl" && reflect.DeepEqual(args, []string{"--context", "admin@prod", "--request-timeout=20s", "get", "namespace", "longhorn-system", "--output=json"})
	if !allowed || !active(ctx) {
		return nil, errors.New("fixed namespace observation command is unverified")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	var output boundedOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	command.WaitDelay = 5 * time.Second
	if err := command.Run(); err != nil || !active(ctx) || output.exceeded {
		return nil, errors.New("fixed namespace observation command did not complete")
	}
	return append([]byte(nil), output.Bytes()...), nil
}

func namespaceIdentity(raw []byte) (string, error) {
	if len(raw) > 2*1024*1024 || strictjson.Validate(raw, map[string][]string{
		"":         {"apiVersion", "kind", "metadata", "status"},
		"metadata": {"name", "namespace", "uid", "resourceVersion", "deletionTimestamp"},
		"status":   {"phase"},
	}) != nil {
		return "", errors.New("native namespace response is incomplete or ambiguous")
	}
	var namespace struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string  `json:"name"`
			Namespace string  `json:"namespace"`
			UID       string  `json:"uid"`
			Version   string  `json:"resourceVersion"`
			Deletion  *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &namespace) != nil || namespace.APIVersion != "v1" || namespace.Kind != "Namespace" || namespace.Metadata.Name != "longhorn-system" || namespace.Metadata.Namespace != "" || namespace.Metadata.UID == "" || len(namespace.Metadata.UID) > 128 || namespace.Metadata.Version == "" || namespace.Metadata.Deletion != nil || namespace.Status.Phase != "Active" {
		return "", errors.New("fixed active Longhorn namespace identity is unverified")
	}
	return namespace.Metadata.UID, nil
}

func runMain() error {
	dispatch, err := namespaceinventory.AuthorizeProtectedDispatch(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	guard := func() error {
		_, err := fixedCommand(ctx, "bash", "./scripts/verify-prod-recovery-source.sh", dispatch.Source)
		return err
	}
	if err := guard(); err != nil {
		return err
	}
	client, endpoint, err := namespaceinventory.LoadProtectedClient(os.Getenv)
	if err != nil {
		return err
	}
	raw, err := fixedCommand(ctx, "kubectl", "--context", "admin@prod", "--request-timeout=20s", "get", "namespace", "longhorn-system", "--output=json")
	if err != nil {
		return err
	}
	uid, err := namespaceIdentity(raw)
	if err != nil {
		return err
	}
	if _, err := namespaceinventory.CalibrateLonghornSystem(ctx, client, endpoint, uid); err != nil {
		return err
	}
	if err := guard(); err != nil {
		return err
	}
	if !active(ctx) {
		return errors.New("namespace metadata observation expired")
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Schema             int    `json:"schema"`
		Source             string `json:"source"`
		Run                string `json:"run"`
		MetadataComplete   bool   `json:"metadataComplete"`
		NamespaceStable    bool   `json:"namespaceStable"`
		DeletionAuthorized bool   `json:"deletionAuthorized"`
	}{1, dispatch.Source, dispatch.Run, true, true, false})
}

func main() {
	if runMain() != nil {
		fmt.Fprintln(os.Stderr, "namespace metadata observation held; no receipt was accepted")
		os.Exit(1)
	}
}
