package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"time"
)

type kubernetes interface {
	call(context.Context, []byte, ...string) ([]byte, error)
}
type kubectl struct{}
type cappedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		b.exceeded = true
		return 0, errors.New("response-limit")
	}
	return b.Buffer.Write(p)
}
func (kubectl) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context=admin@prod", "--request-timeout=20s"}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	var out cappedBuffer
	cmd.Stdout = &out
	if cmd.Run() != nil || out.exceeded {
		return nil, errors.New("kubernetes")
	}
	return out.Bytes(), nil
}

type ownedReference struct{ kind, name, namespace, uid string }

func (r ownedReference) key() string { return r.kind + "/" + r.namespace + "/" + r.name }

type recoveryDriver struct {
	i                     invocation
	k                     kubernetes
	refs                  map[string]ownedReference
	uncertain             bool
	started               bool
	stage                 string
	volumeName, volumeUID string
	pvUID                 string
	source                sourceIdentity
}

func (d *recoveryDriver) lookup(ctx context.Context, kind, name, ns string) (object, bool, error) {
	args := []string{"get", kind, name, "--ignore-not-found=true", "-o=json"}
	if ns != "" {
		args = append(args, "--namespace="+ns)
	}
	data, err := d.k.call(ctx, nil, args...)
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, nil
	}
	var o object
	if json.Unmarshal(data, &o) != nil || len(child(o, "metadata")) == 0 {
		return nil, false, errors.New("resource-response")
	}
	return o, true, nil
}
func (d *recoveryDriver) owned(ctx context.Context, ref ownedReference) (object, error) {
	tracked, ok := d.refs[ref.key()]
	if !ok || tracked != ref || !d.i.permitsMutation(ref.kind, ref.namespace, ref.name) {
		return nil, errors.New("unowned-resource")
	}
	o, found, err := d.lookup(ctx, ref.kind, ref.name, ref.namespace)
	if err != nil || !found {
		return nil, errors.New("owned-observation")
	}
	m := child(o, "metadata")
	if text(m, "uid") != ref.uid || text(child(m, "labels"), recoveryLabel) != d.i.run {
		d.uncertain = true
		return nil, errors.New("replacement")
	}
	return o, nil
}
func (d *recoveryDriver) create(ctx context.Context, o object) (ownedReference, error) {
	m := child(o, "metadata")
	ref := ownedReference{kind: text(o, "kind"), name: text(m, "name"), namespace: text(m, "namespace")}
	if d.uncertain || !d.i.permitsMutation(ref.kind, ref.namespace, ref.name) || text(child(m, "labels"), recoveryLabel) != d.i.run {
		return ref, errors.New("mutation-scope")
	}
	data, err := json.Marshal(o)
	if err != nil {
		return ref, errors.New("manifest")
	}
	response, err := d.k.call(ctx, data, "create", "-f=-", "-o=json")
	if err != nil {
		d.uncertain = true
		return ref, errors.New("create-uncertain")
	}
	var actual object
	if json.Unmarshal(response, &actual) != nil {
		d.uncertain = true
		return ref, errors.New("create-response")
	}
	am := child(actual, "metadata")
	ref.uid = text(am, "uid")
	if ref.uid == "" || text(actual, "kind") != ref.kind || text(am, "name") != ref.name || text(am, "namespace") != ref.namespace || text(child(am, "labels"), recoveryLabel) != d.i.run {
		d.uncertain = true
		return ref, errors.New("create-identity")
	}
	d.refs[ref.key()] = ref
	if !admittedRecoveryObject(o, actual) {
		d.uncertain = true
		return ref, errors.New("admission")
	}
	return ref, nil
}

// Admission must not turn the fixed recovery reader into a different workload.
// Kubernetes may add scheduling defaults, but cannot add a container, hook,
// credential volume, network privilege or different executable to this proof.
func admittedRecoveryObject(want, actual object) bool {
	switch text(want, "kind") {
	case "Pod":
		a, w := child(actual, "spec"), child(want, "spec")
		if a["automountServiceAccountToken"] != false || a["hostNetwork"] == true || a["hostPID"] == true || a["hostIPC"] == true || len(items(a, "initContainers")) != 0 || len(items(a, "ephemeralContainers")) != 0 || text(a, "restartPolicy") != "Never" {
			return false
		}
		for _, key := range []string{"securityContext", "volumes"} {
			if !reflect.DeepEqual(a[key], w[key]) {
				return false
			}
		}
		ac, wc := items(a, "containers"), items(w, "containers")
		if len(ac) != 1 || len(wc) != 1 {
			return false
		}
		for _, key := range []string{"name", "image", "command", "args", "env", "envFrom", "securityContext", "volumeMounts", "volumeDevices", "lifecycle", "startupProbe", "readinessProbe", "livenessProbe"} {
			if !reflect.DeepEqual(ac[0][key], wc[0][key]) {
				return false
			}
		}
	case "CiliumNetworkPolicy":
		return reflect.DeepEqual(actual["spec"], want["spec"])
	case "StorageClass":
		for _, key := range []string{"provisioner", "parameters", "reclaimPolicy", "volumeBindingMode"} {
			if !reflect.DeepEqual(actual[key], want[key]) {
				return false
			}
		}
	case "PersistentVolumeClaim":
		for _, key := range []string{"storageClassName", "selector", "accessModes", "resources"} {
			if !reflect.DeepEqual(child(actual, "spec")[key], child(want, "spec")[key]) {
				return false
			}
		}
	}
	return true
}

func (d *recoveryDriver) resourcePath(ref ownedReference) (string, error) {
	if !d.i.permitsMutation(ref.kind, ref.namespace, ref.name) {
		return "", errors.New("delete-scope")
	}
	api, plural := "/api/v1", map[string]string{"Namespace": "namespaces", "Pod": "pods", "Secret": "secrets", "ConfigMap": "configmaps", "PersistentVolumeClaim": "persistentvolumeclaims"}[ref.kind]
	switch ref.kind {
	case "StorageClass":
		api = "/apis/storage.k8s.io/v1"
		plural = "storageclasses"
	case "CiliumNetworkPolicy":
		api = "/apis/cilium.io/v2"
		plural = "ciliumnetworkpolicies"
	case "Backup":
		api = "/apis/velero.io/v1"
		plural = "backups"
	case "DeleteBackupRequest":
		api = "/apis/velero.io/v1"
		plural = "deletebackuprequests"
	case "DataDownload":
		api = "/apis/velero.io/v2alpha1"
		plural = "datadownloads"
	}
	if plural == "" {
		return "", errors.New("delete-kind")
	}
	if ref.namespace != "" {
		api += "/namespaces/" + ref.namespace
	}
	return api + "/" + plural + "/" + ref.name, nil
}
func (d *recoveryDriver) remove(ctx context.Context, ref ownedReference) error {
	if ref.kind == "Backup" {
		return errors.New("native-backup-retention")
	}
	if d.uncertain {
		return errors.New("retained-uncertain")
	}
	if _, err := d.owned(ctx, ref); err != nil {
		return err
	}
	path, err := d.resourcePath(ref)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": ref.uid}, "propagationPolicy": "Foreground"})
	if _, err = d.k.call(ctx, body, "delete", "--raw="+path, "-f=-"); err != nil {
		d.uncertain = true
		return errors.New("delete-uncertain")
	}
	err = waitRecovery(ctx, 3*time.Minute, func() (bool, error) {
		o, found, e := d.lookup(ctx, ref.kind, ref.name, ref.namespace)
		if e != nil {
			return false, e
		}
		if found && text(child(o, "metadata"), "uid") != ref.uid {
			d.uncertain = true
			return false, errors.New("delete-replacement")
		}
		return !found, nil
	})
	if err == nil {
		delete(d.refs, ref.key())
	}
	return err
}
func waitRecovery(ctx context.Context, limit time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		done, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("observation-timeout")
		}
		select {
		case <-ctx.Done():
			return errors.New("cancelled")
		case <-time.After(3 * time.Second):
		}
	}
}
