package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type retainedVolumeWitness struct{ ReaderUID, ClaimUID, PVUID string }

func runRetainedVolumeProtection(ctx context.Context, env func(string) string, k kubernetes, verifyMain func(context.Context, string) error, output io.Writer) int {
	stage := "dispatch"
	fail := func() int {
		if output != nil {
			_, _ = fmt.Fprintf(output, "EMAIL_RETAINED_VOLUME=UNVERIFIED stage=%s reader=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED\n", stage)
		}
		return 2
	}
	witness := retainedVolumeWitness{env("EMAIL_RETAINED_READER_UID"), env("EMAIL_RETAINED_CLAIM_UID"), env("EMAIL_RETAINED_PV_UID")}
	if output == nil || verifyMain == nil || authorizeRetainedVolumeProtection(env, witness) != nil {
		return fail()
	}
	if _, err := fmt.Fprintln(output, "EMAIL_RETAINED_VOLUME=STARTED source_writes=0 reader=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED"); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stage = "source"
	if verifyMain(ctx, env("GITHUB_SHA")) != nil {
		return fail()
	}
	native := &retainedVolumeKubernetes{inner: k}
	d := &recoveryDriver{i: retainedRecovery, k: native}
	var err error
	d.source, err = d.readSource(ctx)
	if err != nil {
		return fail()
	}
	stage = "binding"
	original, err := d.readRetainedVolume(ctx, witness)
	if err != nil || verifyMain(ctx, env("GITHUB_SHA")) != nil {
		return fail()
	}
	fresh, err := d.readRetainedVolume(ctx, witness)
	if err != nil || !sameRetainedVolumeInputs(original, fresh) || d.sourceRetained(ctx) != nil {
		return fail()
	}
	expected, err := cloneRetainedVolumeObjects(fresh)
	if err != nil {
		return fail()
	}
	child(expected[2], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	state := "EXISTING"
	if text(child(fresh[2], "spec"), "persistentVolumeReclaimPolicy") == "Delete" {
		m := child(fresh[2], "metadata")
		payload, err := json.Marshal([]object{
			{"op": "test", "path": "/metadata/uid", "value": witness.PVUID},
			{"op": "test", "path": "/metadata/name", "value": text(m, "name")},
			{"op": "test", "path": "/metadata/resourceVersion", "value": text(m, "resourceVersion")},
			{"op": "test", "path": "/spec", "value": child(fresh[2], "spec")},
			{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
		})
		if err != nil {
			return fail()
		}
		stage = "mutation"
		native.name, native.patch = text(m, "name"), payload
		_, err = native.call(ctx, payload, "patch", "PersistentVolume", native.name, "--type=json", "--patch-file=/dev/stdin", "-o=json")
		native.patch = nil
		// An uncertain response never retries or reclassifies a mutation as
		// success. No rollback can turn an already-protected PV back to Delete.
		if err != nil {
			return fail()
		}
		state = "UPDATED"
	}
	stage = "readback"
	if verifyMain(ctx, env("GITHUB_SHA")) != nil {
		return fail()
	}
	after, err := d.readRetainedVolume(ctx, witness)
	if err != nil || !sameRetainedVolumeInputs(expected, after) || d.sourceRetained(ctx) != nil {
		return fail()
	}
	if _, err := fmt.Fprintf(output, "EMAIL_RETAINED_VOLUME=PROTECTED state=%s source_writes=0 reader=RETAINED restart=NOT_AUTHORIZED recovery=UNVERIFIED\n", state); err != nil {
		return 2
	}
	return 0
}

func authorizeRetainedVolumeProtection(env func(string) string, witness retainedVolumeWitness) error {
	for key, want := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform",
		"GITHUB_ACTOR": "devantler", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF":           "devantler-tech/platform/.github/workflows/protect-retained-email-volume.yaml@refs/heads/main",
		"EMAIL_RETAINED_VOLUME_CONFIRM": "protect-retained-email-volume",
	} {
		if env(key) != want {
			return errors.New("retained-volume-dispatch")
		}
	}
	if !recoveryRunPattern.MatchString(env("GITHUB_RUN_ID")) || !recoveryRevisionPattern.MatchString(env("GITHUB_SHA")) || !validRetainedVolumeWitness(witness) {
		return errors.New("retained-volume-dispatch")
	}
	return nil
}

// This operation reads no Secret and cannot stop a reader or retire storage.
// Its only mutation is the exact JSON patch produced after final binding.
type retainedVolumeKubernetes struct {
	inner kubernetes
	name  string
	patch []byte
}

func (k *retainedVolumeKubernetes) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, errors.New("retained-volume-capability") }
	if k.inner == nil || len(args) < 2 {
		return fail()
	}
	var raw []byte
	var err error
	if args[0] == "get" && (args[1] == "Pod" || args[1] == "PersistentVolumeClaim" || args[1] == "PersistentVolume") {
		raw, err = (readerInputKubernetes{inner: k.inner}).call(ctx, input, args...)
	} else if len(k.patch) != 0 && bytes.Equal(input, k.patch) && reflect.DeepEqual(args, []string{"patch", "PersistentVolume", k.name, "--type=json", "--patch-file=/dev/stdin", "-o=json"}) {
		raw, err = k.inner.call(ctx, input, args...)
	} else {
		return fail()
	}
	if err != nil || len(raw) > 4<<20 || (len(bytes.TrimSpace(raw)) != 0 && strictjson.Validate(raw, nil) != nil) {
		return fail()
	}
	return raw, nil
}

func (d *recoveryDriver) readRetainedVolume(ctx context.Context, witness retainedVolumeWitness) ([]object, error) {
	fail := func() ([]object, error) { return nil, errors.New("retained-volume-read") }
	reader, found, err := d.lookup(ctx, "Pod", "reader", retainedRecovery.namespace)
	if err != nil || !found || text(child(reader, "metadata"), "uid") != witness.ReaderUID {
		return fail()
	}
	claim, found, err := d.lookup(ctx, "PersistentVolumeClaim", "data", retainedRecovery.namespace)
	if err != nil || !found || text(child(claim, "metadata"), "uid") != witness.ClaimUID {
		return fail()
	}
	name := text(child(claim, "spec"), "volumeName")
	if !kubernetesName.MatchString(name) || name == d.source.pv {
		return fail()
	}
	volume, found, err := d.lookup(ctx, "PersistentVolume", name, "")
	if err != nil || !found || qualifyRetainedVolume(reader, claim, volume, d.source, d.image, witness) != nil {
		return fail()
	}
	return []object{reader, claim, volume}, nil
}

func cloneRetainedVolumeObjects(objects []object) ([]object, error) {
	raw, err := json.Marshal(objects)
	var copy []object
	if err != nil || json.Unmarshal(raw, &copy) != nil {
		return nil, errors.New("retained-volume-snapshot")
	}
	return copy, nil
}

func sameRetainedVolumeInputs(before, after []object) bool {
	if len(before) != 3 || len(after) != 3 {
		return false
	}
	for index := range before {
		if !reflect.DeepEqual(retainedMutableFields(before[index]), retainedMutableFields(after[index])) || !reflect.DeepEqual(child(child(before[index], "metadata"), "annotations"), child(child(after[index], "metadata"), "annotations")) {
			return false
		}
	}
	return true
}

func validRetainedVolumeWitness(witness retainedVolumeWitness) bool {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for _, uid := range []string{witness.ReaderUID, witness.ClaimUID, witness.PVUID} {
		if !uuid.MatchString(uid) || seen[uid] {
			return false
		}
		seen[uid] = true
	}
	return true
}

func qualifyRetainedVolume(reader, claim, volume object, source sourceIdentity, image string, witness retainedVolumeWitness) error {
	fail := func() error { return errors.New("retained-volume-binding") }
	if !validRetainedVolumeWitness(witness) {
		return fail()
	}
	name := text(child(claim, "spec"), "volumeName")
	if source.podUID == "" || source.claimUID == "" || source.pv == "" || source.pvUID == "" || source.podUID == witness.ReaderUID || source.claimUID == witness.ClaimUID || source.pvUID == witness.PVUID || source.pv == name || !regexp.MustCompile(`^pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(name) {
		return fail()
	}
	for _, target := range []struct {
		actual                     object
		kind, name, namespace, uid string
	}{{reader, "Pod", "reader", retainedRecovery.namespace, witness.ReaderUID}, {claim, "PersistentVolumeClaim", "data", retainedRecovery.namespace, witness.ClaimUID}, {volume, "PersistentVolume", name, "", witness.PVUID}} {
		m := child(target.actual, "metadata")
		if text(target.actual, "apiVersion") != "v1" || text(target.actual, "kind") != target.kind || text(m, "name") != target.name || text(m, "namespace") != target.namespace || text(m, "uid") != target.uid || !regexp.MustCompile(`^[1-9][0-9]{0,31}$`).MatchString(text(m, "resourceVersion")) || m["deletionTimestamp"] != nil {
			return fail()
		}
		if owners := m["ownerReferences"]; owners != nil {
			list, ok := owners.([]any)
			if !ok || len(list) != 0 {
				return fail()
			}
		}
		if target.namespace != "" && text(child(m, "labels"), recoveryLabel) != retainedRecovery.run {
			return fail()
		}
	}
	if !admittedRecoveryObject(recoveryReader(retainedRecovery, image), reader) || child(reader, "spec")["terminationGracePeriodSeconds"] != float64(30) || text(child(reader, "status"), "phase") != "Running" {
		return fail()
	}
	ready := false
	for _, condition := range items(child(reader, "status"), "conditions") {
		ready = ready || text(condition, "type") == "Ready" && text(condition, "status") == "True"
	}
	claimSpec := child(claim, "spec")
	if !ready || text(child(claim, "status"), "phase") != "Bound" || text(claimSpec, "storageClassName") != retainedRecovery.namespace || !reflect.DeepEqual(claimSpec["accessModes"], []any{"ReadWriteOnce"}) || text(child(child(claimSpec, "resources"), "requests"), "storage") != "2Gi" {
		return fail()
	}
	spec := child(volume, "spec")
	ref, csi := child(spec, "claimRef"), child(spec, "csi")
	key := child(csi, "nodeStageSecretRef")
	policy := text(spec, "persistentVolumeReclaimPolicy")
	if text(child(volume, "status"), "phase") != "Bound" || text(spec, "storageClassName") != retainedRecovery.namespace || text(ref, "apiVersion") != "v1" || text(ref, "kind") != "PersistentVolumeClaim" || text(ref, "name") != "data" || text(ref, "namespace") != retainedRecovery.namespace || text(ref, "uid") != witness.ClaimUID || text(csi, "driver") != "driver.longhorn.io" || text(csi, "volumeHandle") != name || text(child(csi, "volumeAttributes"), "encrypted") != "true" || text(key, "name") != "storage-key" || text(key, "namespace") != retainedRecovery.namespace || policy != "Delete" && policy != "Retain" {
		return fail()
	}
	return nil
}
