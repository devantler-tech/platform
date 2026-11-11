package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const retainV1SourceOperation = "retain-v1-source"

var retentionDigest = regexp.MustCompile("^[a-f0-9]{64}$")

type retentionTarget struct {
	namespace, claim, digest string
}

func parseRetentionTarget(approval, prefix string) (retentionTarget, error) {
	fields := strings.Split(approval, ":")
	if len(fields) == 3 && fields[0] == prefix && retentionDigest.MatchString(fields[2]) {
		namespace, claim, joined := strings.Cut(fields[1], "/")
		if joined && namespace != "" && namespace != "velero" && namespace != "longhorn-system" &&
			!strings.HasPrefix(namespace, "longhorn-") && !strings.HasPrefix(namespace, "email-data-recovery-") &&
			safeKubeRequest("persistentvolumeclaim", namespace, claim, false) == nil {
			return retentionTarget{namespace, claim, fields[2]}, nil
		}
	}
	return retentionTarget{}, errors.New("an exact bound application claim and its approved storage identity digest are required")
}

func authorizeV1SourceRetention(getenv func(string) string) error {
	approval := getenv("LONGHORN_V2_CONFIRM")
	if getenv("LONGHORN_V2_OPERATION") != retainV1SourceOperation || getenv("LONGHORN_V2_NODE") != "" {
		return errors.New("the explicit source retention operation is required without a worker selection")
	}
	if _, err := parseRetentionTarget(approval, "retain-longhorn-v1-source"); err != nil {
		return err
	}
	return authorizeWorkflow(getenv, approval)
}

// A backup-owned claim is outside the migration path, including when a DataUpload
// is configured to expose its snapshot outside the default backup namespace.
func hasDataMoverOwner(claim object) bool {
	raw := value(claim, "metadata", "ownerReferences")
	if raw == nil {
		return false
	}
	owners, valid := raw.([]any)
	if !valid {
		return true
	}
	for _, rawOwner := range owners {
		owner, valid := rawOwner.(object)
		if !valid || text(owner, "kind") == "" || text(owner, "apiVersion") == "" ||
			(text(owner, "kind") == "DataUpload" && strings.HasPrefix(text(owner, "apiVersion"), "velero.io/")) {
			return true
		}
	}
	return false
}

func (c client) observeRetainedSource(ctx context.Context, target retentionTarget) (object, error) {
	unproven := errors.New("the approved application claim, bound PV and original V1 backend are not jointly proven")
	claim, err := c.get(ctx, "persistentvolumeclaim", target.namespace, target.claim, false)
	if err != nil || text(claim, "apiVersion") != "v1" || text(claim, "kind") != "PersistentVolumeClaim" ||
		text(claim, "metadata", "namespace") != target.namespace || text(claim, "metadata", "name") != target.claim ||
		value(claim, "metadata", "deletionTimestamp") != nil || text(claim, "status", "phase") != "Bound" || hasDataMoverOwner(claim) {
		return nil, unproven
	}
	claimUID := text(claim, "metadata", "uid")
	pvName := text(claim, "spec", "volumeName")
	class := text(claim, "spec", "storageClassName")
	if pvName != "pvc-"+claimUID || (class != "longhorn" && class != "longhorn-wffc") {
		return nil, unproven
	}
	pv, err := c.get(ctx, "persistentvolume", "", pvName, false)
	if err != nil || text(pv, "apiVersion") != "v1" || text(pv, "kind") != "PersistentVolume" ||
		text(pv, "metadata", "name") != pvName || text(pv, "metadata", "namespace") != "" ||
		value(pv, "metadata", "deletionTimestamp") != nil || text(pv, "status", "phase") != "Bound" ||
		text(pv, "spec", "claimRef", "namespace") != target.namespace ||
		text(pv, "spec", "claimRef", "name") != target.claim || text(pv, "spec", "claimRef", "uid") != claimUID ||
		text(pv, "spec", "storageClassName") != class || text(pv, "spec", "csi", "driver") != "driver.longhorn.io" ||
		text(pv, "spec", "csi", "volumeHandle") != pvName {
		return nil, unproven
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", pvName, false)
	if err != nil || text(volume, "apiVersion") != "longhorn.io/v1beta2" || text(volume, "kind") != "Volume" ||
		text(volume, "metadata", "namespace") != "longhorn-system" || text(volume, "metadata", "name") != pvName ||
		value(volume, "metadata", "deletionTimestamp") != nil || text(volume, "spec", "dataEngine") != "v1" ||
		text(volume, "status", "kubernetesStatus", "pvName") != pvName {
		return nil, unproven
	}
	identity := strings.Join([]string{target.namespace + "/" + target.claim, claimUID, pvName,
		text(pv, "metadata", "uid"), text(volume, "metadata", "name"), text(volume, "metadata", "uid")}, ":")
	if fmt.Sprintf("%x", sha256.Sum256([]byte(identity))) != target.digest {
		return nil, unproven
	}
	return pv, nil
}

func retainV1Source(ctx context.Context, c client, source, approval string) error {
	target, err := parseRetentionTarget(approval, "retain-longhorn-v1-source")
	if err != nil {
		return err
	}
	return retainObservedSource(ctx, c, source, "LONGHORN_V1_SOURCE_RETENTION", func(ctx context.Context) (object, error) {
		return c.observeRetainedSource(ctx, target)
	})
}

func retainObservedSource(ctx context.Context, c client, source, marker string, observe func(context.Context) (object, error)) error {
	if c.context != "admin@prod" {
		return errors.New("the protected production context is required")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	pv, err := observe(ctx)
	if err != nil {
		return err
	}
	switch text(pv, "spec", "persistentVolumeReclaimPolicy") {
	case "Retain":
		fmt.Printf("%s=RETAINED writes=0\n", marker)
		return nil
	case "Delete":
	default:
		return errors.New("the original PV does not have a supported reclaim policy")
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	// A lost response is uncertain. Read the same identity back once and never
	// repeat the write in this run, including if the API returned a conflict.
	writeErr := c.patch(ctx, "persistentvolume", "", pv,
		object{"op": "test", "path": "/spec/claimRef", "value": value(pv, "spec", "claimRef")},
		object{"op": "test", "path": "/spec/csi/driver", "value": text(pv, "spec", "csi", "driver")},
		object{"op": "test", "path": "/spec/csi/volumeHandle", "value": text(pv, "spec", "csi", "volumeHandle")},
		object{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"},
		object{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"})
	current, err := observe(ctx)
	if err != nil || text(current, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
		return errors.New("source retention is unconfirmed, preserve the claim and investigate before proceeding")
	}
	if writeErr != nil {
		fmt.Printf("%s=RETAINED write=observed\n", marker)
		return nil
	}
	fmt.Printf("%s=RETAINED writes=1\n", marker)
	return nil
}
