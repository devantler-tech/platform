package main

import (
	"strings"
	"testing"
)

const csiDeletionFinalizer = "external-provisioner.volume.kubernetes.io/finalizer"

// TestStorageProtectionAcceptsOnlyItsCSIReconciliation reproduces the CSI
// controller's asynchronous finalizer removal without changing our emitted patch.
func TestStorageProtectionAcceptsOnlyItsCSIReconciliation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*storageAPI)
		wantOK bool
	}{
		{"controller removes deletion finalizer", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection", "external-attacher/driver-longhorn-io"}
		}, true},
		{"PV protection removed too", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{"external-attacher/driver-longhorn-io"}
		}, false},
		{"attacher removed too", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection"}
		}, false},
		{"foreign finalizer added", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection", "external-attacher/driver-longhorn-io", "foreign.example/finalizer"}
		}, false},
		{"storage source changed too", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection", "external-attacher/driver-longhorn-io"}
			mchild(mchild(mitem(a.volumes, 0), "spec"), "csi")["volumeHandle"] = "replacement"
		}, false},
		{"claim metadata changed too", func(a *storageAPI) {
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection", "external-attacher/driver-longhorn-io"}
			mchild(mitem(a.claims, 0), "metadata")["labels"] = map[string]any{"foreign": "changed"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := storageFixture(t)
			mchild(mitem(a.volumes, 0), "metadata")["finalizers"] = []any{csiDeletionFinalizer, "kubernetes.io/pv-protection", "external-attacher/driver-longhorn-io"}
			changed := false
			a.onRead = func(a *storageAPI, _ string) {
				if a.patches == 1 && !changed {
					changed = true
					test.change(a)
				}
			}
			out, err := executeStorage(t, a, testEnvironment())
			if (err == nil) != test.wantOK {
				t.Fatalf("controller transition: error=%v, want success=%t", err, test.wantOK)
			}
			if test.wantOK && (a.patches != 2 || a.pauses != 2 || !strings.Contains(out, `"result":"PROTECTED"`)) {
				t.Fatalf("missing separated protection receipt: patches=%d pauses=%d output=%s", a.patches, a.pauses, out)
			}
			if !test.wantOK && (a.patches != 1 || out != "") {
				t.Fatalf("unsafe drift continued or reported success: patches=%d output=%s", a.patches, out)
			}
		})
	}
}

// TestStorageProtectionRejectsFinalizerDriftBeforeItsWrite ensures a controller
// transition cannot excuse changes to Delete or already-retained volumes.
func TestStorageProtectionRejectsFinalizerDriftBeforeItsWrite(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			a := storageFixture(t)
			v := mitem(a.volumes, 0)
			mchild(v, "spec")["persistentVolumeReclaimPolicy"] = policy
			mchild(v, "metadata")["finalizers"] = []any{csiDeletionFinalizer, "kubernetes.io/pv-protection"}
			a.onRead = func(a *storageAPI, _ string) {
				if a.collections >= 2 {
					mchild(v, "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection"}
				}
			}
			out, err := executeStorage(t, a, testEnvironment())
			if err == nil || a.patches != 0 || out != "" {
				t.Fatalf("pre-write drift accepted: error=%v patches=%d output=%s", err, a.patches, out)
			}
		})
	}
}

// TestStorageProtectionRejectsFinalizerReappearance requires the accepted
// one-way removal to remain absent in later samples.
func TestStorageProtectionRejectsFinalizerReappearance(t *testing.T) {
	a := storageFixture(t)
	v := mitem(a.volumes, 0)
	mchild(v, "metadata")["finalizers"] = []any{csiDeletionFinalizer, "kubernetes.io/pv-protection"}
	a.onRead = func(a *storageAPI, _ string) {
		if a.patches == 1 {
			mchild(v, "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection"}
		}
		if a.patches == 2 {
			mchild(v, "metadata")["finalizers"] = []any{csiDeletionFinalizer, "kubernetes.io/pv-protection"}
		}
	}
	out, err := executeStorage(t, a, testEnvironment())
	if err == nil || a.patches != 2 || out != "" {
		t.Fatalf("finalizer reappearance accepted: error=%v patches=%d output=%s", err, a.patches, out)
	}
}
