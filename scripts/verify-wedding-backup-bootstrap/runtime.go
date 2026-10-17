package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type client struct {
	ctx     context.Context
	run     string
	command func(context.Context, []string, []byte) ([]byte, error)
}

func esoPolicy(run, nsUID string) (object, error) {
	if !digits.MatchString(run) || !uuid.MatchString(nsUID) {
		return nil, refused
	}
	ns := "wedding-bootstrap-" + run
	m := meta(ns, "external-secrets", run)
	m["ownerReferences"] = []any{object{"apiVersion": "v1", "kind": "Namespace", "name": ns, "uid": nsUID}}
	return object{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": m, "spec": object{"endpointSelector": object{"matchLabels": object{"app.kubernetes.io/name": "external-secrets"}}, "egress": []any{object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": ns, "app.kubernetes.io/name": "openbao", ownerKey: run}}}, "toPorts": []any{object{"ports": []any{object{"port": "8200", "protocol": "TCP"}}}}}}}}, nil
}

func (k client) call(ns string, input []byte, args ...string) ([]byte, error) {
	if !digits.MatchString(k.run) || (ns != "" && ns != "wedding-bootstrap-"+k.run && ns != "external-secrets") {
		return nil, refused
	}
	prefix := []string{"--context", "admin@prod", "--request-timeout=30s"}
	if ns != "" {
		prefix = append(prefix, "--namespace", ns)
	}
	return k.command(k.ctx, append(prefix, args...), input)
}
func (k client) get(ns, kind, name string) (object, error) {
	b, err := k.call(ns, nil, "get", kind, name, "--ignore-not-found=true", "-o", "json")
	if err != nil {
		return nil, refused
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var o object
	if json.Unmarshal(b, &o) != nil || o == nil {
		return nil, refused
	}
	return o, nil
}
func (k client) create(o object) error {
	if str(o, "metadata", "namespace") != "wedding-bootstrap-"+k.run && !(str(o, "kind") == "Namespace" && str(o, "metadata", "name") == "wedding-bootstrap-"+k.run) && !(str(o, "kind") == "CiliumNetworkPolicy" && str(o, "metadata", "namespace") == "external-secrets" && str(o, "metadata", "name") == "wedding-bootstrap-"+k.run) {
		return refused
	}
	b, err := json.Marshal(o)
	if err != nil {
		return refused
	}
	_, err = k.call("", b, "create", "-f", "-", "-o", "name")
	return err
}
func owned(o object, name, ns, run string) bool {
	return o != nil && str(o, "metadata", "name") == name && str(o, "metadata", "namespace") == ns && str(o, "metadata", "labels", ownerKey) == run && uuid.MatchString(str(o, "metadata", "uid"))
}
func removeOwned(k client, ns, kind, name, expectedUID string) error {
	o, err := k.get(ns, kind, name)
	if err != nil {
		return err
	}
	if o == nil {
		return nil
	}
	if !owned(o, name, ns, k.run) || (expectedUID != "" && str(o, "metadata", "uid") != expectedUID) {
		return refused
	}
	var url string
	switch kind {
	case "namespaces":
		if ns != "" || name != "wedding-bootstrap-"+k.run {
			return refused
		}
		url = "/api/v1/namespaces/" + name
	case "secrets":
		if ns != "wedding-bootstrap-"+k.run || name != projectedSecret {
			return refused
		}
		url = "/api/v1/namespaces/" + ns + "/secrets/" + name
	case "ciliumnetworkpolicies":
		if ns != "external-secrets" || name != "wedding-bootstrap-"+k.run {
			return refused
		}
		url = "/apis/cilium.io/v2/namespaces/" + ns + "/ciliumnetworkpolicies/" + name
	default:
		return refused
	}
	body, _ := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": str(o, "metadata", "uid")}, "propagationPolicy": "Foreground"})
	if _, err = k.call("", body, "delete", "--raw="+url, "-f", "-"); err != nil {
		return refused
	}
	// ESO can recreate a Secret immediately. Wait for the deleted UID, rather
	// than waiting forever for its replacement to disappear by name.
	return waitFor(k, func() (bool, error) {
		current, e := k.get(ns, kind, name)
		return current == nil || str(current, "metadata", "uid") != str(o, "metadata", "uid"), e
	})
}
func cleanup(k client, nsUID string) error {
	ns := "wedding-bootstrap-" + k.run
	n, err := k.get("", "namespaces", ns)
	if err != nil {
		return err
	}
	if n != nil && (!owned(n, ns, "", k.run) || (nsUID != "" && str(n, "metadata", "uid") != nsUID)) {
		return refused
	}
	if n != nil {
		nsUID = str(n, "metadata", "uid")
	}
	p, err := k.get("external-secrets", "ciliumnetworkpolicies", ns)
	if err != nil {
		return err
	}
	if p != nil {
		refs, _ := at(p, "metadata", "ownerReferences").([]any)
		if !owned(p, ns, "external-secrets", k.run) || len(refs) != 1 {
			return refused
		}
		owner, ok := refs[0].(map[string]any)
		if !ok || str(owner, "kind") != "Namespace" || str(owner, "name") != ns || !uuid.MatchString(str(owner, "uid")) || (nsUID != "" && str(owner, "uid") != nsUID) {
			return refused
		}
		if err = removeOwned(k, "external-secrets", "ciliumnetworkpolicies", ns, str(p, "metadata", "uid")); err != nil {
			return err
		}
	}
	if err = removeOwned(k, "", "namespaces", ns, nsUID); err != nil {
		return err
	}
	for _, target := range [][3]string{{"", "namespaces", ns}, {"external-secrets", "ciliumnetworkpolicies", ns}} {
		o, err := k.get(target[0], target[1], target[2])
		if err != nil || o != nil {
			return refused
		}
	}
	return nil
}

type bao struct {
	url  string
	ctx  context.Context
	http *http.Client
}

func (b bao) request(method, path, token string, input object, want int) (object, error) {
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[1-9][0-9]{0,4}$`).MatchString(b.url) || !strings.HasPrefix(path, "/v1/") {
		return nil, refused
	}
	payload, _ := json.Marshal(input)
	if input == nil {
		payload = nil
	}
	req, err := http.NewRequestWithContext(b.ctx, method, b.url+path, bytes.NewReader(payload))
	if err != nil {
		return nil, refused
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, refused
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if err != nil || len(body) > 1048576 || resp.StatusCode != want {
		return nil, refused
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return object{}, nil
	}
	var result object
	if json.Unmarshal(body, &result) != nil {
		return nil, refused
	}
	return result, nil
}
func (b bao) initialize() (string, error) {
	init, err := b.request("PUT", "/v1/sys/init", "", object{"secret_shares": 1, "secret_threshold": 1}, 200)
	if err != nil {
		return "", err
	}
	keys, _ := init["keys_base64"].([]any)
	root := str(init, "root_token")
	if len(keys) != 1 || root == "" {
		return "", refused
	}
	key, ok := keys[0].(string)
	if !ok || key == "" {
		return "", refused
	}
	unsealed, err := b.request("PUT", "/v1/sys/unseal", "", object{"key": key}, 200)
	if err != nil || unsealed["sealed"] != false {
		return "", refused
	}
	if _, err = b.request("POST", "/v1/sys/mounts/secret", root, object{"type": "kv", "options": object{"version": "2"}}, 204); err != nil {
		return "", err
	}
	policy := `path "secret/data/apps/wedding-app/backup/r2" { capabilities = ["create", "update", "read", "delete"] }
path "secret/metadata/apps/wedding-app/backup/r2" { capabilities = ["create", "update", "read", "delete", "list"] }
path "auth/token/lookup-self" { capabilities = ["read"] }`
	if _, err = b.request("PUT", "/v1/sys/policies/acl/wedding-fixture", root, object{"policy": policy}, 204); err != nil {
		return "", err
	}
	token, err := b.request("POST", "/v1/auth/token/create", root, object{"policies": []any{"wedding-fixture"}, "no_parent": true, "no_default_policy": true, "ttl": "1h", "explicit_max_ttl": "1h", "renewable": false}, 200)
	if err != nil {
		return "", err
	}
	if _, err = b.request("POST", "/v1/auth/token/revoke-self", root, nil, 204); err != nil {
		return "", err
	}
	value := str(token, "auth", "client_token")
	if value == "" {
		return "", refused
	}
	return value, nil
}

func forward(ctx context.Context, run string) (string, func(), error) {
	if !digits.MatchString(run) {
		return "", nil, refused
	}
	fctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(fctx, "kubectl", "--context", "admin@prod", "--namespace", "wedding-bootstrap-"+run, "port-forward", "--address=127.0.0.1", "pod/openbao", "0:8200")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	cmd.Stderr = io.Discard
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, refused
	}
	if cmd.Start() != nil {
		cancel()
		return "", nil, refused
	}
	ports := make(chan string, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 4096)
		re := regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:([0-9]+) -> 8200$`)
		for scanner.Scan() {
			match := re.FindStringSubmatch(scanner.Text())
			if len(match) == 2 {
				p, e := strconv.Atoi(match[1])
				if e == nil && p > 0 && p < 65536 {
					select {
					case ports <- match[1]:
					default:
					}
				}
			}
		}
		_ = cmd.Wait()
	}()
	stop := func() { cancel(); <-finished }
	select {
	case p := <-ports:
		return "http://127.0.0.1:" + p, stop, nil
	case <-finished:
		cancel()
		return "", nil, refused
	case <-ctx.Done():
		stop()
		return "", nil, refused
	case <-time.After(45 * time.Second):
		stop()
		return "", nil, refused
	}
}
func waitFor(k client, predicate func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(k.ctx, 5*time.Minute)
	defer cancel()
	for {
		ok, err := predicate()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return refused
		case <-time.After(2 * time.Second):
		}
	}
}
func ready(o object) bool {
	conditions, _ := at(o, "status", "conditions").([]any)
	for _, v := range conditions {
		c, _ := v.(map[string]any)
		if c["type"] == "Ready" && c["status"] == "True" {
			return true
		}
	}
	return false
}
func number(o object, keys ...string) int { n, _ := at(o, keys...).(float64); return int(n) }

func newClient(ctx context.Context, run string) client {
	return client{ctx: ctx, run: run, command: func(ctx context.Context, args []string, input []byte) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		cmd.Stderr = io.Discard
		var stdout bytes.Buffer
		cmd.Stdout = &limitedWriter{writer: &stdout, remaining: 1048576}
		if cmd.Run() != nil {
			return nil, refused
		}
		return stdout.Bytes(), nil
	}}
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("output limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= n
	return n, err
}
