package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const retainProviderSourceOperation = "retain-provider-source"

func parseProviderRetentionTarget(approval string) (retentionTarget, error) {
	target, err := parseRetentionTarget(approval, "retain-hetzner-source")
	if err != nil {
		return retentionTarget{}, err
	}
	switch target.namespace {
	case "observability", "openbao", "kube-system":
		return target, nil
	default:
		return retentionTarget{}, errors.New("the approved claim is outside the provider migration namespaces")
	}
}

func authorizeProviderSourceRetention(getenv func(string) string) error {
	approval := getenv("LONGHORN_V2_CONFIRM")
	if getenv("LONGHORN_V2_OPERATION") != retainProviderSourceOperation || getenv("LONGHORN_V2_NODE") != "" {
		return errors.New("the explicit provider source retention operation is required without a worker selection")
	}
	if _, err := parseProviderRetentionTarget(approval); err != nil {
		return err
	}
	return authorizeWorkflow(getenv, approval)
}

func providerRetentionDigest(bound providerVolumeClaim) string {
	identity := strings.Join([]string{hetznerCSIDriver, bound.Namespace + "/" + bound.Claim, bound.ClaimUID,
		bound.PV, bound.PVUID, bound.Handle, strconv.FormatUint(bound.RequestedBytes, 10)}, ":")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

func (c client) observeProviderRetainedSource(ctx context.Context, target retentionTarget) (object, error) {
	unverified := errors.New("the approved provider PVC, bound PV and backend identity are unverified")
	claim, err := c.get(ctx, "persistentvolumeclaim", target.namespace, target.claim, false)
	if err != nil || text(claim, "apiVersion") != "v1" || text(claim, "kind") != "PersistentVolumeClaim" ||
		text(claim, "metadata", "namespace") != target.namespace || text(claim, "metadata", "name") != target.claim ||
		text(claim, "spec", "storageClassName") != "hcloud" || hasDataMoverOwner(claim) {
		return nil, unverified
	}
	pvName := text(claim, "spec", "volumeName")
	if pvName != "pvc-"+text(claim, "metadata", "uid") {
		return nil, unverified
	}
	pv, err := c.get(ctx, "persistentvolume", "", pvName, false)
	if err != nil || text(pv, "apiVersion") != "v1" || text(pv, "kind") != "PersistentVolume" ||
		text(pv, "metadata", "name") != pvName || text(pv, "metadata", "namespace") != "" {
		return nil, unverified
	}
	claims, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
	if err != nil || len(claims) != 1 || providerRetentionDigest(claims[0]) != target.digest {
		return nil, unverified
	}
	class, err := c.get(ctx, "storageclass", "", "hcloud", false)
	if err != nil || text(class, "apiVersion") != "storage.k8s.io/v1" || text(class, "kind") != "StorageClass" ||
		text(class, "metadata", "name") != "hcloud" || text(class, "metadata", "namespace") != "" ||
		value(class, "metadata", "deletionTimestamp") != nil || text(class, "provisioner") != hetznerCSIDriver {
		return nil, unverified
	}
	return pv, nil
}

func retainProviderSource(ctx context.Context, c client, source, approval string) error {
	target, err := parseProviderRetentionTarget(approval)
	if err != nil {
		return err
	}
	return retainObservedSource(ctx, c, source, "LONGHORN_PROVIDER_SOURCE_RETENTION", func(ctx context.Context) (object, error) {
		return c.observeProviderRetainedSource(ctx, target)
	})
}
