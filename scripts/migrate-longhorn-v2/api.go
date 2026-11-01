package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type command func(context.Context, []byte, string, ...string) ([]byte, error)

// errConflict reports an API 409, the one failure a caller may retry with fresh state.
var errConflict = errors.New("external command refused: the resource changed")

func execute(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Stdin = bytes.NewReader(input)
	// Host configuration and provider replies never enter public workflow logs:
	// stderr is only classified, never returned or printed.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	result, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "Error from server (Conflict)") {
			return nil, errConflict
		}
		return nil, errors.New("external command failed")
	}
	return result, nil
}

type client struct {
	run     command
	context string
}

var kubeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
var kubeNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func safeKubeRequest(kind, namespace, name string, list bool) error {
	validNamespace := namespace == "" || list && namespace == "*" || kubeNamespace.MatchString(namespace)
	if len(kind) > 253 || !kubeName.MatchString(kind) || len(namespace) > 63 || !validNamespace || (!list && (len(name) > 253 || !kubeName.MatchString(name))) {
		return errors.New("resource identity cannot select an option, path or another request scope")
	}
	return nil
}

func (c client) kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return c.run(ctx, input, "kubectl", append([]string{"--context=" + c.context, "--request-timeout=20s"}, args...)...)
}

func namespaceArgs(namespace string) []string {
	if namespace == "*" {
		return []string{"--all-namespaces"}
	}
	if namespace != "" {
		return []string{"--namespace=" + namespace}
	}
	return nil
}

func (c client) list(ctx context.Context, kind, namespace string) ([]object, error) {
	if err := safeKubeRequest(kind, namespace, "", true); err != nil {
		return nil, err
	}
	args := append([]string{"get", kind, "--output=json"}, namespaceArgs(namespace)...)
	b, err := c.kubectl(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return decodeList(b)
}

func (c client) get(ctx context.Context, kind, namespace, name string, optional bool) (object, error) {
	if err := safeKubeRequest(kind, namespace, name, false); err != nil {
		return nil, err
	}
	args := append([]string{"get", kind, name, "--output=json"}, namespaceArgs(namespace)...)
	if optional {
		args = append(args, "--ignore-not-found")
	}
	b, err := c.kubectl(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	if optional && len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var o object
	if json.Unmarshal(b, &o) != nil || text(o, "metadata", "uid") == "" || text(o, "metadata", "resourceVersion") == "" {
		return nil, errors.New("resource identity is incomplete")
	}
	return o, nil
}

func (c client) snapshot(ctx context.Context) (inventory, error) {
	s := inventory{}
	for _, query := range []struct {
		kind, namespace string
		result          *[]object
	}{
		{"nodes", "", &s.nodes},
		{"nodes.longhorn.io", "longhorn-system", &s.storageNodes},
		{"volumes.longhorn.io", "longhorn-system", &s.volumes},
		{"replicas.longhorn.io", "longhorn-system", &s.replicas},
		{"engines.longhorn.io", "longhorn-system", &s.engines},
		{"orphans.longhorn.io", "longhorn-system", &s.orphans},
		{"pods", "*", &s.pods},
	} {
		objects, err := c.list(ctx, query.kind, query.namespace)
		if err != nil {
			return inventory{}, err
		}
		*query.result = objects
	}
	return s, nil
}

func identityTests(o object) []object {
	return []object{
		{"op": "test", "path": "/metadata/uid", "value": text(o, "metadata", "uid")},
		{"op": "test", "path": "/metadata/resourceVersion", "value": text(o, "metadata", "resourceVersion")},
	}
}

func (c client) patch(ctx context.Context, kind, namespace string, o object, changes ...object) error {
	if text(o, "metadata", "uid") == "" || text(o, "metadata", "resourceVersion") == "" {
		return errors.New("refusing mutation without resource identity")
	}
	b, err := json.Marshal(append(identityTests(o), changes...))
	if err != nil {
		return err
	}
	args := append([]string{"patch", kind, text(o, "metadata", "name"), "--type=json", "--patch-file=/dev/stdin", "--field-manager=platform-longhorn-v2-migration", "--output=name"}, namespaceArgs(namespace)...)
	_, err = c.kubectl(ctx, b, args...)
	return err
}

func sourceCurrent(ctx context.Context, run command, sha string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := run(ctx, nil, "git", "ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main")
	if err != nil || strings.TrimSpace(string(b)) != sha+"\trefs/heads/main" {
		return errors.New("the reviewed source is no longer current main")
	}
	return nil
}

func (c client) verifyHost(ctx context.Context, address string, n object) error {
	host, err := c.talosResource(ctx, address, "systeminformation", "systeminformation")
	if err != nil || text(host, "metadata", "namespace") != "hardware" || text(host, "spec", "uuid") == "" ||
		!strings.EqualFold(text(host, "spec", "uuid"), text(n, "status", "nodeInfo", "systemUUID")) {
		return errors.New("host endpoint does not match the observed Kubernetes worker hardware")
	}
	return nil
}

func (c client) talosResource(ctx context.Context, address, kind, name string) (object, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := c.run(ctx, nil, "talosctl", "--nodes="+address, "get", kind, name, "--output=json")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	var o object
	if decoder.Decode(&o) != nil || text(o, "metadata", "id") != name {
		return nil, errors.New("talos volume identity is incomplete")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("talos volume response is not singular")
	}
	return o, nil
}
