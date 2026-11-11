package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
)

// Disposal is distinct from the lost original trial. It preserves all source
// messages and the backup and never emits the fresh recovery PASS receipt.
func runRetainedDisposal(ctx context.Context, env func(string) string, k kubernetes, output io.Writer) int {
	stage := "dispatch"
	cleanup := "NOT_STARTED"
	fail := func() int {
		_, _ = fmt.Fprintf(output, "EMAIL_RETAINED_DISPOSAL=UNVERIFIED stage=%s cleanup=%s\n", stage, cleanup)
		return 2
	}
	if namespaceinventory.AuthorizeRetainedEmailDispatch(env) != nil {
		return fail()
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	readerUID, claimUID := env("EMAIL_RETAINED_READER_UID"), env("EMAIL_RETAINED_CLAIM_UID")
	if !uuid.MatchString(readerUID) || !uuid.MatchString(claimUID) || readerUID == claimUID {
		return fail()
	}
	if _, err := fmt.Fprintln(output, "EMAIL_RETAINED_DISPOSAL=STARTED source_writes=0 original_trial=UNVERIFIED"); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	stage = "credentials"
	client, endpoint, err := namespaceinventory.LoadRetainedEmailClient(env)
	if err != nil {
		return fail()
	}
	d := &recoveryDriver{i: retainedRecovery, k: k, refs: map[string]ownedReference{}}
	stage = "source"
	if d.source, err = d.readSource(ctx); err != nil {
		return fail()
	}
	if d.namespacePolicies, err = loadNamespacePolicies(); err != nil {
		return fail()
	}
	stage = "retained-graph"
	if err = d.adoptRetainedGraph(ctx, readerUID, claimUID); err != nil {
		return fail()
	}
	if err = d.observeRestoredStorage(ctx); err != nil {
		return fail()
	}
	if err = d.retainedSnapshot(ctx, ""); err != nil {
		return fail()
	}
	stage = "complete-inventory"
	// The first complete read freezes controller-created auxiliary identities.
	// Later reads cannot adopt newly appeared or replaced resources.
	type auxiliaryIdentity struct{ uid, parent string }
	auxiliary := map[string]auxiliaryIdentity{}
	frozen := false
	d.completeNamespaceInventory = func(ctx context.Context) error {
		ns, e := d.ref("Namespace", d.i.namespace, "")
		if e != nil {
			return e
		}
		inventory, e := namespaceinventory.ObserveRetainedEmailRecovery(ctx, client, endpoint, ns.uid)
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		bindings := map[string]namespaceinventory.Binding{}
		for _, binding := range inventory.Bindings() {
			if retainedTransientResource(binding) {
				continue
			}
			if !retainedInventoryResource(binding) {
				return errors.New("inventory-foreign-resource")
			}
			key := binding.Kind + "/" + binding.Namespace + "/" + binding.Name
			if _, duplicate := bindings[key]; duplicate {
				return errors.New("inventory-duplicate-name")
			}
			bindings[key] = binding
		}
		for _, binding := range bindings {
			ref := ownedReference{kind: binding.Kind, name: binding.Name, namespace: binding.Namespace, uid: binding.UID}
			key := ref.key()
			seen[key] = true
			if expected, owned := d.refs[key]; owned {
				if ref != expected {
					return errors.New("inventory-replaced")
				}
				continue
			}
			allowed := (ref.kind == "ServiceAccount" && ref.name == "default") || (ref.kind == "ConfigMap" && ref.name == "kube-root-ca.crt")
			if _, known := d.namespacePolicies[ref.kind+"/"+ref.name]; known {
				allowed = true
			}
			parent := ""
			if ref.kind == "CiliumEndpoint" || ref.kind == "PolicyReport" {
				parent, e = d.retainedAuxiliary(ctx, binding, bindings, readerUID)
				if e != nil {
					return e
				}
				allowed = true
			}
			identity := auxiliaryIdentity{ref.uid, parent}
			if !allowed || ref.namespace != d.i.namespace || (frozen && auxiliary[key] != identity) {
				return errors.New("inventory-foreign")
			}
			auxiliary[key] = identity
		}
		for key, ref := range d.refs {
			if ref.namespace == d.i.namespace && !seen[key] {
				return errors.New("inventory-missing")
			}
		}
		for key, identity := range auxiliary {
			if !seen[key] {
				if identity.parent == "Pod/"+d.i.namespace+"/reader" {
					if _, readerPresent := d.refs[identity.parent]; !readerPresent {
						continue
					}
				}
				return errors.New("inventory-auxiliary-changed")
			}
		}
		frozen = true
		return nil
	}
	if err = d.cleanupPreflight(ctx); err != nil {
		return fail()
	}
	stage = "read-only-message"
	secret, found, err := d.lookup(ctx, "Secret", "private-mail-maintainer", "private-mail")
	if err != nil || !found {
		return fail()
	}
	password, err := base64.StdEncoding.DecodeString(text(child(secret, "data"), "password"))
	if err != nil || len(password) < 16 || len(password) > 4096 {
		return fail()
	}
	sourceEndpoint, stopSource, err := d.forward(ctx, "private-mail", "private-mail-0", d.source.podUID)
	if err != nil {
		return fail()
	}
	defer stopSource()
	readerEndpoint, stopReader, err := d.forward(ctx, d.i.namespace, "reader", readerUID)
	if err != nil {
		return fail()
	}
	defer stopReader()
	source, err := newRetainedMailboxClient(sourceEndpoint, string(password))
	if err != nil {
		return fail()
	}
	reader, err := newRetainedMailboxClient(readerEndpoint, string(password))
	if err != nil {
		return fail()
	}
	if _, err = verifyRetainedMessage(ctx, reader, source); err != nil {
		return fail()
	}
	if err = d.sourceRetained(ctx); err != nil || ctx.Err() != nil {
		return fail()
	}
	if _, err = fmt.Fprintln(output, "EMAIL_RETAINED_COPY=VERIFIED source_writes=0 original_trial=UNVERIFIED"); err != nil {
		return 2
	}
	stopReader()
	stopSource()
	stage = "owned-disposal"
	cleanup = "UNVERIFIED"
	cleanupCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer finish()
	if err = d.cleanupRestore(cleanupCtx); err != nil {
		return fail()
	}
	if _, err = fmt.Fprintln(output, "EMAIL_RETAINED_DISPOSAL=PASS source_writes=0 source=RETAINED backup=RETAINED original_trial=UNVERIFIED"); err != nil {
		return 2
	}
	return 0
}

// retainedTransientResource reports an Event, from either Event API. The
// kubelet and controllers record Events for the reader's own lifecycle at any
// time; they hold no recovered data and expire on their own, so they are neither
// foreign resources nor identities to freeze between inventory reads.
func retainedTransientResource(binding namespaceinventory.Binding) bool {
	return binding.Kind == "Event" && (binding.Resource == "events" || binding.Resource == "events.events.k8s.io")
}

func retainedInventoryResource(binding namespaceinventory.Binding) bool {
	want := map[string]string{"Pod": "pods", "PersistentVolumeClaim": "persistentvolumeclaims", "Secret": "secrets", "ConfigMap": "configmaps", "ServiceAccount": "serviceaccounts", "CiliumNetworkPolicy": "ciliumnetworkpolicies.cilium.io", "NetworkPolicy": "networkpolicies.networking.k8s.io", "CiliumEndpoint": "ciliumendpoints.cilium.io", "PolicyReport": "policyreports.wgpolicyk8s.io"}
	return want[binding.Kind] != "" && binding.Resource == want[binding.Kind]
}

// Bind the retained transfer to the controller-owned volume snapshot. The
// creation time is only a controller chronology check, never marker freshness.
func (d *recoveryDriver) retainedSnapshot(ctx context.Context, sourceClaimUID string) error {
	backup, err := d.ref("Backup", d.i.namespace+"-backup", "velero")
	if err != nil {
		return err
	}
	b, err := d.owned(ctx, backup)
	if err != nil {
		return err
	}
	data, err := d.k.call(ctx, nil, "get", "podvolumebackups.velero.io", "--namespace=velero", "-o=json", "--selector=velero.io/backup-uid="+backup.uid)
	if err != nil {
		return errors.New("retained-snapshot-list")
	}
	var list object
	if json.Unmarshal(data, &list) != nil || text(child(list, "metadata"), "continue") != "" || child(list, "metadata")["remainingItemCount"] != nil {
		return errors.New("retained-snapshot-list")
	}
	raw, ok := list["items"].([]any)
	volumes := items(list, "items")
	if !ok || len(raw) != len(volumes) || len(volumes) > 256 {
		return errors.New("retained-snapshot-list")
	}
	volume, err := sourceVolumeBackup(volumes, d.source.volume)
	if err != nil {
		return err
	}
	if sourceClaimUID != "" && text(child(child(volume, "metadata"), "labels"), "velero.io/pvc-uid") != sourceClaimUID {
		return errors.New("retained-snapshot-source-claim")
	}
	sourceAtBackup := d.source
	sourceAtBackup.podUID = text(child(child(volume, "spec"), "pod"), "uid")
	created, err := time.Parse(time.RFC3339, text(child(b, "metadata"), "creationTimestamp"))
	if err != nil {
		return errors.New("retained-snapshot-time")
	}
	snapshot, err := proveBackup(b, volume, sourceAtBackup, d.i, backup.uid, created)
	if err != nil {
		return err
	}
	transfer, err := d.ref("DataDownload", d.i.namespace+"-download", "velero")
	if err != nil {
		return err
	}
	live, err := d.owned(ctx, transfer)
	if err != nil || text(child(live, "spec"), "snapshotID") != snapshot {
		return errors.New("retained-snapshot-transfer")
	}
	return nil
}
