package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"
)

type kubernetes interface {
	call(context.Context, []byte, ...string) ([]byte, error)
}
type kubectl struct{}

// Tool failures may quote credentials or private topology. Return a closed code only.
func (kubectl) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	argv := append([]string{"--context=admin@prod", "--request-timeout=20s"}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	var out cappedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("kubernetes")
	}
	if out.exceeded {
		return nil, errors.New("response-limit")
	}
	return out.Bytes(), nil
}

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

type reference struct{ kind, name, namespace, uid string }

func (r reference) key() string { return r.kind + "/" + r.namespace + "/" + r.name }
func (r reference) path() (string, error) {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(r.name) {
		return "", errors.New("resource-name")
	}
	if r.namespace != "" && !regexp.MustCompile(`^email-storage-canary-[1-9][0-9]{0,19}$`).MatchString(r.namespace) {
		return "", errors.New("namespace")
	}
	switch r.kind {
	case "Namespace":
		if r.namespace != "" || !strings.HasPrefix(r.name, "email-storage-canary-") {
			break
		}
		return "/api/v1/namespaces/" + r.name, nil
	case "StorageClass":
		if r.namespace != "" || !strings.HasPrefix(r.name, "email-storage-canary-") {
			break
		}
		return "/apis/storage.k8s.io/v1/storageclasses/" + r.name, nil
	case "Pod", "Secret", "PersistentVolumeClaim", "NetworkPolicy":
		if r.namespace == "" {
			break
		}
		plural := map[string]string{"Pod": "pods", "Secret": "secrets", "PersistentVolumeClaim": "persistentvolumeclaims", "NetworkPolicy": "networkpolicies"}[r.kind]
		prefix := "/api/v1"
		if r.kind == "NetworkPolicy" {
			prefix = "/apis/networking.k8s.io/v1"
		}
		return prefix + "/namespaces/" + r.namespace + "/" + plural + "/" + r.name, nil
	}
	return "", errors.New("resource-kind")
}

type runner struct {
	k                                   kubernetes
	run, namespace, storageClass, stage string
	refs                                map[string]reference
	ambiguous                           bool
	volumeName, volumeUID               string
}

func (r *runner) lookup(ctx context.Context, kind, name, ns string) (object, bool, error) {
	args := []string{"get", kind, name, "--ignore-not-found", "-o=json"}
	if ns != "" {
		args = append(args, "--namespace="+ns)
	}
	data, err := r.k.call(ctx, nil, args...)
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, nil
	}
	var o object
	if json.Unmarshal(data, &o) != nil || len(child(o, "metadata")) == 0 {
		return nil, false, errors.New("response")
	}
	return o, true, nil
}
func (r *runner) create(ctx context.Context, o object) (reference, error) {
	meta := child(o, "metadata")
	ref := reference{kind: text(o, "kind"), name: text(meta, "name"), namespace: text(meta, "namespace")}
	if _, err := ref.path(); err != nil {
		return ref, err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return ref, errors.New("manifest")
	}
	response, err := r.k.call(ctx, data, "create", "-f=-", "-o=json")
	if err != nil {
		r.ambiguous = true
		return ref, errors.New("create-uncertain")
	}
	var live object
	if json.Unmarshal(response, &live) != nil {
		return ref, r.uncertain()
	}
	actual := child(live, "metadata")
	ref.uid = text(actual, "uid")
	if text(live, "kind") != ref.kind || text(actual, "name") != ref.name || text(actual, "namespace") != ref.namespace || text(child(actual, "labels"), labelKey) != r.run || ref.uid == "" {
		return ref, r.uncertain()
	}
	r.refs[ref.key()] = ref
	return ref, nil
}
func (r *runner) uncertain() error { r.ambiguous = true; return errors.New("identity-uncertain") }
func (r *runner) owned(ctx context.Context, ref reference) (object, error) {
	live, found, err := r.lookup(ctx, ref.kind, ref.name, ref.namespace)
	if err != nil || !found {
		return nil, errors.New("owned-resource")
	}
	meta := child(live, "metadata")
	if text(meta, "uid") != ref.uid || text(child(meta, "labels"), labelKey) != r.run {
		return nil, errors.New("ownership")
	}
	return live, nil
}
func waitFor(ctx context.Context, limit time.Duration, check func() (bool, error)) error {
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
func (r *runner) remove(ctx context.Context, ref reference) error {
	if tracked, ok := r.refs[ref.key()]; !ok || tracked != ref {
		return errors.New("unowned-delete")
	}
	if _, err := r.owned(ctx, ref); err != nil {
		return err
	}
	path, err := ref.path()
	if err != nil {
		return err
	}
	body, err := deletionBody(ref.uid)
	if err != nil {
		return err
	}
	if _, err = r.k.call(ctx, body, "delete", "--raw="+path, "-f=-"); err != nil {
		return errors.New("delete-uncertain")
	}
	err = waitFor(ctx, 3*time.Minute, func() (bool, error) {
		live, found, err := r.lookup(ctx, ref.kind, ref.name, ref.namespace)
		if err != nil {
			return false, err
		}
		if found && text(child(live, "metadata"), "uid") != ref.uid {
			return false, errors.New("replacement")
		}
		return !found, nil
	})
	if err == nil {
		delete(r.refs, ref.key())
	}
	return err
}
func (r *runner) preflight(ctx context.Context) error {
	hr, found, err := r.lookup(ctx, "helmrelease", "longhorn", "longhorn-system")
	if err != nil || !found {
		return errors.New("storage-version")
	}
	if text(child(child(child(hr, "spec"), "chart"), "spec"), "version") != "1.13.0" || child(hr, "status")["observedGeneration"] != child(hr, "metadata")["generation"] {
		return errors.New("storage-version")
	}
	history := items(child(hr, "status"), "history")
	if len(history) == 0 || text(history[0], "chartVersion") != "1.13.0" || text(child(hr, "status"), "lastAttemptedRevision") != "1.13.0" {
		return errors.New("storage-release")
	}
	ready := false
	for _, c := range items(child(hr, "status"), "conditions") {
		if text(c, "type") == "Ready" && text(c, "status") == "True" {
			ready = true
		}
	}
	if !ready {
		return errors.New("storage-readiness")
	}
	raw, err := r.k.call(ctx, nil, "get", "nodes", "-o=json")
	if err != nil {
		return err
	}
	var list object
	if json.Unmarshal(raw, &list) != nil {
		return errors.New("node-response")
	}
	nodes := items(list, "items")
	if len(nodes) < 3 {
		return errors.New("nodes")
	}
	for _, node := range nodes {
		if text(child(child(node, "status"), "nodeInfo"), "osImage") != "Talos (v1.13.11)" {
			return errors.New("os-version")
		}
		ready = false
		for _, c := range items(child(node, "status"), "conditions") {
			if text(c, "type") == "Ready" && text(c, "status") == "True" {
				ready = true
			}
		}
		if !ready {
			return errors.New("node-readiness")
		}
	}
	return nil
}
func (r *runner) bindVolume(ctx context.Context, claim reference) error {
	return waitFor(ctx, 3*time.Minute, func() (bool, error) {
		pvc, err := r.owned(ctx, claim)
		if err != nil {
			return false, err
		}
		spec := child(pvc, "spec")
		if text(spec, "storageClassName") != r.storageClass {
			return false, errors.New("claim-class")
		}
		if text(child(pvc, "status"), "phase") != "Bound" {
			return false, nil
		}
		name := text(spec, "volumeName")
		if name != "pvc-"+claim.uid || !regexp.MustCompile(`^pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(name) {
			return false, errors.New("claim-volume")
		}
		pv, found, err := r.lookup(ctx, "persistentvolume", name, "")
		if err != nil || !found {
			return false, errors.New("pv")
		}
		ps := child(pv, "spec")
		cr := child(ps, "claimRef")
		csi := child(ps, "csi")
		if text(cr, "uid") != claim.uid || text(cr, "namespace") != r.namespace || text(cr, "name") != "probe" || text(ps, "storageClassName") != r.storageClass || text(ps, "persistentVolumeReclaimPolicy") != "Delete" || text(csi, "driver") != "driver.longhorn.io" || text(csi, "volumeHandle") != name {
			return false, errors.New("pv-binding")
		}
		for _, key := range []string{"nodeStageSecretRef", "nodePublishSecretRef", "nodeExpandSecretRef"} {
			s := child(csi, key)
			if text(s, "name") != "encryption-key" || text(s, "namespace") != r.namespace {
				return false, errors.New("pv-key")
			}
		}
		volume, found, err := r.lookup(ctx, "volumes.longhorn.io", name, "longhorn-system")
		if err != nil || !found {
			return false, errors.New("native-volume")
		}
		uid := text(child(volume, "metadata"), "uid")
		if uid == "" {
			return false, errors.New("native-volume-identity")
		}
		if r.volumeUID != "" && (r.volumeUID != uid || r.volumeName != name) {
			return false, errors.New("native-volume-replaced")
		}
		if err = checkVolume(volume, name, uid, false); err != nil {
			return false, err
		}
		r.volumeName, r.volumeUID = name, uid
		return text(child(volume, "status"), "robustness") == "healthy", nil
	})
}
func (r *runner) detached(ctx context.Context) error {
	if r.volumeUID == "" {
		return errors.New("unbound-volume")
	}
	return waitFor(ctx, 3*time.Minute, func() (bool, error) {
		volume, found, err := r.lookup(ctx, "volumes.longhorn.io", r.volumeName, "longhorn-system")
		if err != nil || !found {
			return false, errors.New("native-volume")
		}
		if checkVolume(volume, r.volumeName, r.volumeUID, false) != nil {
			return false, errors.New("native-volume-binding")
		}
		if checkVolume(volume, r.volumeName, r.volumeUID, true) != nil {
			return false, nil
		}
		raw, err := r.k.call(ctx, nil, "get", "volumeattachments.storage.k8s.io", "-o=json")
		if err != nil {
			return false, err
		}
		var list object
		if json.Unmarshal(raw, &list) != nil {
			return false, errors.New("attachment-response")
		}
		for _, attachment := range items(list, "items") {
			if text(child(child(attachment, "spec"), "source"), "persistentVolumeName") == r.volumeName {
				return false, nil
			}
		}
		return true, nil
	})
}
func (r *runner) positive(ctx context.Context, phase string) (reference, error) {
	ref, err := r.create(ctx, podManifest(r.namespace, phase, r.run))
	if err != nil {
		return ref, err
	}
	err = waitFor(ctx, 4*time.Minute, func() (bool, error) {
		pod, err := r.owned(ctx, ref)
		if err != nil {
			return false, err
		}
		if !sameProbeSpec(pod, podManifest(r.namespace, phase, r.run)) {
			return false, errors.New("probe-definition")
		}
		state := text(child(pod, "status"), "phase")
		if state == "Failed" {
			return false, errors.New("probe-failed")
		}
		if state != "Succeeded" {
			return false, nil
		}
		logs, err := r.k.call(ctx, nil, "logs", ref.name, "--namespace="+r.namespace, "--container=probe", "--limit-bytes=1024")
		if err != nil {
			return false, err
		}
		if !probeSucceeded(pod, ref.uid, string(logs), phase) {
			return false, errors.New("probe-receipt")
		}
		return true, nil
	})
	return ref, err
}
func (r *runner) negative(ctx context.Context, phase string) (reference, error) {
	ref, err := r.create(ctx, podManifest(r.namespace, phase, r.run))
	if err != nil {
		return ref, err
	}
	first := time.Time{}
	err = waitFor(ctx, 3*time.Minute, func() (bool, error) {
		pod, err := r.owned(ctx, ref)
		if err != nil {
			return false, err
		}
		if !sameProbeSpec(pod, podManifest(r.namespace, phase, r.run)) {
			return false, errors.New("probe-definition")
		}
		if p := text(child(pod, "status"), "phase"); p == "Running" || p == "Succeeded" || p == "Failed" {
			return false, errors.New("negative-probe-started")
		}
		raw, err := r.k.call(ctx, nil, "get", "events", "--namespace="+r.namespace, "--field-selector=involvedObject.uid="+ref.uid, "-o=json")
		if err != nil {
			return false, err
		}
		var list object
		if json.Unmarshal(raw, &list) != nil {
			return false, errors.New("event-response")
		}
		if !negativeMount(pod, items(list, "items"), ref.uid, r.volumeName, phase) {
			first = time.Time{}
			return false, nil
		}
		if first.IsZero() {
			first = time.Now()
		}
		return time.Since(first) >= 30*time.Second, nil
	})
	return ref, err
}
func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", errors.New("entropy")
	}
	return hex.EncodeToString(b), nil
}
func (r *runner) execute(ctx context.Context) error {
	r.stage = "versions"
	if err := r.preflight(ctx); err != nil {
		return err
	}
	key, err := randomKey()
	if err != nil {
		return err
	}
	wrong, err := randomKey()
	if err != nil || key == wrong {
		return errors.New("entropy")
	}
	r.stage = "create-test-resources"
	for _, o := range []object{namespaceManifest(r.namespace, r.run), policyManifest(r.namespace, r.run), storageClassManifest(r.storageClass, r.namespace, r.run)} {
		if _, err = r.create(ctx, o); err != nil {
			return err
		}
	}
	secret, err := r.create(ctx, keyManifest(r.namespace, r.run, key))
	if err != nil {
		return err
	}
	claim, err := r.create(ctx, claimManifest(r.namespace, r.storageClass, r.run))
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(claim.uid) {
		return r.uncertain()
	}
	// The standard CSI provisioner names this disposable volume from the claim UID.
	// Retain that identity before waiting for a bind, including interrupted startup.
	r.volumeName = "pvc-" + claim.uid
	r.stage = "write"
	pod, err := r.positive(ctx, "write")
	if err != nil {
		return err
	}
	r.stage = "native-encrypted-volume"
	if err = r.bindVolume(ctx, claim); err != nil {
		return err
	}
	if err = r.remove(ctx, pod); err != nil {
		return err
	}
	if err = r.detached(ctx); err != nil {
		return err
	}
	r.stage = "remount"
	pod, err = r.positive(ctx, "read")
	if err != nil {
		return err
	}
	if err = r.bindVolume(ctx, claim); err != nil {
		return err
	}
	if err = r.remove(ctx, pod); err != nil {
		return err
	}
	if err = r.detached(ctx); err != nil {
		return err
	}
	r.stage = "missing-key"
	if err = r.remove(ctx, secret); err != nil {
		return err
	}
	pod, err = r.negative(ctx, "missing")
	if err != nil {
		return err
	}
	if err = r.remove(ctx, pod); err != nil {
		return err
	}
	if err = r.detached(ctx); err != nil {
		return err
	}
	r.stage = "wrong-key"
	secret, err = r.create(ctx, keyManifest(r.namespace, r.run, wrong))
	if err != nil {
		return err
	}
	pod, err = r.negative(ctx, "wrong")
	if err != nil {
		return err
	}
	if err = r.remove(ctx, pod); err != nil {
		return err
	}
	if err = r.detached(ctx); err != nil {
		return err
	}
	if err = r.remove(ctx, secret); err != nil {
		return err
	}
	r.stage = "recovery"
	if _, err = r.create(ctx, keyManifest(r.namespace, r.run, key)); err != nil {
		return err
	}
	pod, err = r.positive(ctx, "recovery")
	if err != nil {
		return err
	}
	if err = r.bindVolume(ctx, claim); err != nil {
		return err
	}
	if err = r.remove(ctx, pod); err != nil {
		return err
	}
	return r.detached(ctx)
}
func (r *runner) cleanup(ctx context.Context) error {
	if r.ambiguous {
		return errors.New("ambiguous-create")
	}
	for _, kind := range []string{"Pod", "PersistentVolumeClaim", "Secret", "StorageClass", "NetworkPolicy", "Namespace"} {
		for _, ref := range r.refs {
			if ref.kind != kind {
				continue
			}
			if err := r.remove(ctx, ref); err != nil {
				return err
			}
		}
		if kind == "PersistentVolumeClaim" && r.volumeName != "" {
			if err := waitFor(ctx, 3*time.Minute, func() (bool, error) {
				_, pv, err := r.lookup(ctx, "persistentvolume", r.volumeName, "")
				if err != nil {
					return false, err
				}
				_, volume, err := r.lookup(ctx, "volumes.longhorn.io", r.volumeName, "longhorn-system")
				return !pv && !volume, err
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
func main() {
	run, err := authorize(os.Getenv)
	if err != nil || len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "EMAIL_STORAGE_CANARY=UNVERIFIED stage=dispatch cleanup=NOT_STARTED")
		os.Exit(2)
	}
	r := runner{k: kubectl{}, run: run, namespace: "email-storage-canary-" + run, storageClass: "email-storage-canary-" + run, refs: map[string]reference{}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	err = r.execute(ctx)
	cancel()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	cleanupErr := r.cleanup(cleanupCtx)
	cleanupCancel()
	if err != nil || cleanupErr != nil {
		status := "VERIFIED"
		if cleanupErr != nil {
			status = "RETAINED"
		}
		fmt.Fprintf(os.Stderr, "EMAIL_STORAGE_CANARY=UNVERIFIED stage=%s cleanup=%s run=%s\n", r.stage, status, run)
		os.Exit(2)
	}
	fmt.Println("EMAIL_STORAGE_CANARY=PASS storage=1.13.0 os=1.13.11 write=true remount=true missing_key_refused=true wrong_key_refused=true recovered=true cleanup=VERIFIED")
}

// Admission defaults are allowed; changing the executable, data path or access is not.
func sameProbeSpec(actual, expected object) bool {
	a, e := child(actual, "spec"), child(expected, "spec")
	if a["automountServiceAccountToken"] != false || a["hostNetwork"] == true || a["hostPID"] == true || a["hostIPC"] == true || len(items(a, "initContainers")) != 0 || len(items(a, "ephemeralContainers")) != 0 {
		return false
	}
	ac, ec := items(a, "containers"), items(e, "containers")
	av, ev := items(a, "volumes"), items(e, "volumes")
	if len(ac) != 1 || len(ec) != 1 || len(av) != 1 || len(ev) != 1 || !reflect.DeepEqual(av, ev) {
		return false
	}
	if text(ac[0], "name") != "probe" || text(ac[0], "image") != probeImage || !reflect.DeepEqual(ac[0]["command"], ec[0]["command"]) || !reflect.DeepEqual(ac[0]["volumeMounts"], ec[0]["volumeMounts"]) || len(items(ac[0], "env")) != 0 || len(items(ac[0], "envFrom")) != 0 {
		return false
	}
	s := child(a, "securityContext")
	for _, key := range []string{"runAsNonRoot", "runAsUser", "runAsGroup", "fsGroup", "seccompProfile"} {
		if !reflect.DeepEqual(s[key], child(e, "securityContext")[key]) {
			return false
		}
	}
	s = child(ac[0], "securityContext")
	for _, key := range []string{"allowPrivilegeEscalation", "readOnlyRootFilesystem", "capabilities"} {
		if !reflect.DeepEqual(s[key], child(ec[0], "securityContext")[key]) {
			return false
		}
	}
	return true
}
