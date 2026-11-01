package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"time"
)

const clientImage = "docker.io/curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777"
const label = "platform.devantler.tech/email-provider-probe"

//go:embed probe-client.sh
var clientScript string

type object = map[string]any
type api interface {
	call(context.Context, []byte, ...string) ([]byte, error)
}
type reference struct{ Kind, Name, UID string }
type ownership struct {
	Run, SHA string
	Unknown  bool
	Refs     []reference
}
type runner struct {
	k                                api
	run, sha, expectedImage, receipt string
	pause                            func(context.Context, time.Duration) error
	own                              ownership
}

func str(o object, keys ...string) string { value, _ := at(o, keys...).(string); return value }
func at(o object, keys ...string) any {
	var value any = o
	for _, key := range keys {
		current, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = current[key]
	}
	return value
}
func rows(o object, keys ...string) []object {
	a, ok := at(o, keys...).([]any)
	if !ok {
		return nil
	}
	out := make([]object, 0, len(a))
	for _, entry := range a {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil
		}
		out = append(out, m)
	}
	return out
}
func ready(o object) bool {
	for _, c := range rows(o, "status", "conditions") {
		if str(c, "type") == "Ready" && str(c, "status") == "True" {
			return true
		}
	}
	return false
}
func (r *runner) read(ctx context.Context, args ...string) (object, error) {
	data, err := r.k.call(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("read")
	}
	if len(data) == 0 {
		return nil, nil
	}
	var o object
	if json.Unmarshal(data, &o) != nil || o == nil {
		return nil, fmt.Errorf("read")
	}
	return o, nil
}

// Bind the probe to the same ready pod, controller and provider node before and
// after. This deliberately proves no stable address, reverse DNS or delivery.
func (r *runner) source(ctx context.Context) (string, string, error) {
	pod, err := r.read(ctx, "get", "pod", "private-mail-0", "-n", "private-mail", "-o", "json")
	if err != nil || !ready(pod) || str(pod, "status", "phase") != "Running" || str(pod, "metadata", "uid") == "" || at(pod, "metadata", "deletionTimestamp") != nil {
		return "", "", fmt.Errorf("source-pod")
	}
	containers, statuses := rows(pod, "spec", "containers"), rows(pod, "status", "containerStatuses")
	if len(containers) != 1 || len(statuses) != 1 || str(containers[0], "name") != "private-mail" || str(containers[0], "image") != r.expectedImage || str(statuses[0], "name") != "private-mail" || at(statuses[0], "ready") != true || str(statuses[0], "imageID") == "" {
		return "", "", fmt.Errorf("source-image")
	}
	restarts, ok := at(statuses[0], "restartCount").(float64)
	if !ok || restarts < 0 {
		return "", "", fmt.Errorf("source-restarts")
	}
	sts, err := r.read(ctx, "get", "statefulset", "private-mail", "-n", "private-mail", "-o", "json")
	if err != nil || str(sts, "metadata", "uid") == "" || at(sts, "spec", "replicas") != float64(1) || at(sts, "status", "readyReplicas") != float64(1) {
		return "", "", fmt.Errorf("source-controller")
	}
	owners := rows(pod, "metadata", "ownerReferences")
	if len(owners) != 1 || str(owners[0], "kind") != "StatefulSet" || str(owners[0], "name") != "private-mail" || str(owners[0], "uid") != str(sts, "metadata", "uid") || at(owners[0], "controller") != true {
		return "", "", fmt.Errorf("source-owner")
	}
	name := str(pod, "spec", "nodeName")
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`).MatchString(name) {
		return "", "", fmt.Errorf("source-node")
	}
	node, err := r.read(ctx, "get", "node", name, "-o", "json")
	if err != nil || !ready(node) || str(node, "metadata", "uid") == "" || str(node, "metadata", "labels", "kubernetes.io/hostname") != name || !regexp.MustCompile(`^hcloud://[1-9][0-9]*$`).MatchString(str(node, "spec", "providerID")) {
		return "", "", fmt.Errorf("source-node")
	}
	binding := []any{str(pod, "metadata", "uid"), str(sts, "metadata", "uid"), name, str(node, "metadata", "uid"), str(node, "spec", "providerID"), str(statuses[0], "imageID"), restarts}
	return name, string(marshal(binding)), nil
}

func (r *runner) execute(ctx context.Context) (result error) {
	node, before, err := r.source(ctx)
	if err != nil {
		return err
	}
	leftovers, err := r.read(ctx, "get", "pods,ciliumnetworkpolicies", "-n", "whoami", "-l", label, "-o", "json")
	list, ok := at(leftovers, "items").([]any)
	if err != nil || !ok || len(list) != 0 {
		return fmt.Errorf("leftovers")
	}
	r.own = ownership{Run: r.run, SHA: r.sha}
	file, err := os.OpenFile(r.receipt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("receipt")
	}
	if _, err = file.Write(marshal(r.own)); err != nil {
		_ = file.Close()
		return fmt.Errorf("receipt")
	}
	if file.Close() != nil {
		return fmt.Errorf("receipt")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := r.cleanup(cleanupCtx); err != nil {
			result = err
		}
	}()
	policy, pod := r.manifests(node)
	for _, manifest := range []object{policy, pod} {
		if err := r.create(ctx, manifest); err != nil {
			return err
		}
	}
	ref := r.own.Refs[1]
	for {
		observed, err := r.read(ctx, "get", "Pod", ref.Name, "-n", "whoami", "--ignore-not-found", "-o", "json")
		if err != nil || str(observed, "metadata", "uid") != ref.UID {
			return fmt.Errorf("probe-identity")
		}
		phase := str(observed, "status", "phase")
		if phase == "Succeeded" {
			containers := rows(observed, "spec", "containers")
			status := rows(observed, "status", "containerStatuses")
			if len(containers) != 1 || str(containers[0], "image") != clientImage || !reflect.DeepEqual(at(containers[0], "command"), []any{"/bin/sh", "-c", clientScript}) || str(observed, "spec", "nodeName") != node || len(status) != 1 || str(status[0], "name") != "probe" || at(status[0], "restartCount") != float64(0) || at(status[0], "state", "terminated", "exitCode") != float64(0) || str(status[0], "imageID") == "" {
				return fmt.Errorf("probe-status")
			}
			break
		}
		if phase == "Failed" || phase == "Unknown" {
			logs, _ := r.k.call(ctx, nil, "logs", ref.Name, "-n", "whoami", "-c", "probe", "--limit-bytes=128")
			return sessionFailure(logs)
		}
		if err := r.pause(ctx, 2*time.Second); err != nil {
			return fmt.Errorf("probe-deadline")
		}
	}
	logs, err := r.k.call(ctx, nil, "logs", ref.Name, "-n", "whoami", "-c", "probe", "--limit-bytes=128")
	if err != nil || string(logs) != "SMTP_PROVIDER_SESSION=VERIFIED\n" {
		return sessionFailure(logs)
	}
	_, after, err := r.source(ctx)
	if err != nil || before != after {
		return fmt.Errorf("source-changed")
	}
	return nil
}

func sessionFailure(logs []byte) error {
	match := regexp.MustCompile(`^SMTP_PROVIDER_SESSION=UNVERIFIED curl_exit=([0-9]{1,3}) response=([0-9]{3})\n$`).FindSubmatch(logs)
	if len(match) == 3 {
		return fmt.Errorf("probe-session-curl-%s-response-%s", match[1], match[2])
	}
	return fmt.Errorf("probe-session")
}

func (r *runner) persist() error {
	// This runner owns the private, exclusive receipt on the ephemeral runner.
	if err := os.WriteFile(r.receipt, marshal(r.own), 0600); err != nil {
		return fmt.Errorf("receipt")
	}
	return nil
}
func (r *runner) restore() error {
	info, err := os.Lstat(r.receipt)
	if os.IsNotExist(err) {
		r.own = ownership{Run: r.run, SHA: r.sha}
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("receipt")
	}
	data, err := os.ReadFile(r.receipt)
	if err != nil || json.Unmarshal(data, &r.own) != nil || r.own.Run != r.run || r.own.SHA != r.sha || len(r.own.Refs) > 2 {
		return fmt.Errorf("receipt")
	}
	for _, ref := range r.own.Refs {
		if ref.Name != "email-provider-probe-"+r.run || (ref.Kind != "Pod" && ref.Kind != "CiliumNetworkPolicy") || !regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`).MatchString(ref.UID) {
			return fmt.Errorf("receipt")
		}
	}
	return nil
}
func (r *runner) create(ctx context.Context, manifest object) error {
	r.own.Unknown = true
	if err := r.persist(); err != nil {
		return err
	}
	data, err := r.k.call(ctx, marshal(manifest), "create", "-f", "-", "-o", "json")
	var created object
	if err != nil || json.Unmarshal(data, &created) != nil || str(created, "kind") != str(manifest, "kind") || str(created, "metadata", "name") != "email-provider-probe-"+r.run || str(created, "metadata", "namespace") != "whoami" || str(created, "metadata", "labels", label) != r.run || !regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`).MatchString(str(created, "metadata", "uid")) {
		return fmt.Errorf("creation-unknown")
	}
	r.own.Refs = append(r.own.Refs, reference{Kind: str(created, "kind"), Name: str(created, "metadata", "name"), UID: str(created, "metadata", "uid")})
	r.own.Unknown = false
	return r.persist()
}
func (r *runner) cleanup(ctx context.Context) error {
	failed := r.own.Unknown
	for len(r.own.Refs) > 0 {
		i := len(r.own.Refs) - 1
		ref := r.own.Refs[i]
		o, err := r.read(ctx, "get", ref.Kind, ref.Name, "-n", "whoami", "--ignore-not-found", "-o", "json")
		if err != nil || (o != nil && (str(o, "metadata", "uid") != ref.UID || str(o, "metadata", "labels", label) != r.run)) {
			return fmt.Errorf("cleanup-identity")
		}
		if o != nil {
			path := "/api/v1/namespaces/whoami/pods/" + ref.Name
			if ref.Kind == "CiliumNetworkPolicy" {
				path = "/apis/cilium.io/v2/namespaces/whoami/ciliumnetworkpolicies/" + ref.Name
			}
			body := marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": ref.UID}, "propagationPolicy": "Foreground"})
			if _, err = r.k.call(ctx, body, "delete", "--raw="+path, "-f=-"); err != nil {
				return fmt.Errorf("cleanup-delete")
			}
			for {
				o, err = r.read(ctx, "get", ref.Kind, ref.Name, "-n", "whoami", "--ignore-not-found", "-o", "json")
				if err != nil {
					return fmt.Errorf("cleanup-read")
				}
				if o == nil {
					break
				}
				if str(o, "metadata", "uid") != ref.UID {
					return fmt.Errorf("cleanup-identity")
				}
				if r.pause(ctx, time.Second) != nil {
					return fmt.Errorf("cleanup-deadline")
				}
			}
		}
		r.own.Refs = r.own.Refs[:i]
		if r.persist() != nil {
			return fmt.Errorf("receipt")
		}
	}
	if failed {
		return fmt.Errorf("creation-unknown")
	}
	if err := os.Remove(r.receipt); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("receipt")
	}
	return nil
}

func (r *runner) manifests(node string) (object, object) {
	meta := func() object {
		return object{"name": "email-provider-probe-" + r.run, "namespace": "whoami", "labels": object{label: r.run}}
	}
	policy := object{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": meta(), "spec": object{
		"endpointSelector": object{"matchLabels": object{label: r.run}}, "ingress": []any{},
		"egress": []any{
			object{"toEndpoints": []any{object{"matchLabels": object{"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}}, "toPorts": []any{object{"ports": []any{object{"port": "53", "protocol": "ANY"}}, "rules": object{"dns": []any{object{"matchPattern": "*"}}}}}},
			object{"toFQDNs": []any{object{"matchName": "mx01.mail.icloud.com"}}, "toPorts": []any{object{"ports": []any{object{"port": "25", "protocol": "TCP"}}}}},
		}}}
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": meta(), "spec": object{
		"nodeSelector": object{"kubernetes.io/hostname": node}, "automountServiceAccountToken": false, "hostUsers": false, "restartPolicy": "Never", "activeDeadlineSeconds": 180,
		"securityContext": object{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers": []any{object{"name": "probe", "image": clientImage, "command": []any{"/bin/sh", "-c", clientScript},
			"resources":       object{"requests": object{"cpu": "10m", "memory": "16Mi"}, "limits": object{"cpu": "100m", "memory": "32Mi"}},
			"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}}},
	}}
	return policy, pod
}
