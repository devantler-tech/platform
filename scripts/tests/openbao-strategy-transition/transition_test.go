package strategytransition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// This is the actual post-rendered chart, not a second implementation of its patch.
// Every request uses envtest's generated client; no user kubeconfig is loaded.
func TestRenderedStrategyClearsRetainedRollingUpdate(t *testing.T) {
	manifest, err := os.ReadFile(os.Getenv("OPENBAO_TRANSITION_MANIFEST"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.YAMLToJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	desired := &unstructured.Unstructured{}
	if err := desired.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	releaseData, err := os.ReadFile(os.Getenv("OPENBAO_TRANSITION_RELEASE"))
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Spec struct {
			Upgrade struct {
				ServerSideApply string `json:"serverSideApply"`
				Force           bool   `json:"force"`
			} `json:"upgrade"`
			Rollback struct {
				ServerSideApply string `json:"serverSideApply"`
				Force           bool   `json:"force"`
			} `json:"rollback"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(releaseData, &release); err != nil {
		t.Fatal(err)
	}
	if release.Spec.Upgrade.Force || release.Spec.Rollback.Force {
		t.Fatal("force replacement is not an acceptable strategy migration")
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("verified local API assets are required")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory:    os.Getenv("KUBEBUILDER_ASSETS"),
		ControlPlaneStartTimeout: time.Minute,
		ControlPlaneStopTimeout:  time.Minute,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	// Helm receives only this test API's generated identity, never ambient access.
	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"envtest": {
			Server: config.Host, CertificateAuthorityData: config.CAData,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"envtest": {
			ClientCertificateData: config.CertData, ClientKeyData: config.KeyData,
		}},
		Contexts: map[string]*clientcmdapi.Context{"envtest": {
			Cluster: "envtest", AuthInfo: "envtest", Namespace: "openbao",
		}},
		CurrentContext: "envtest",
	}, kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &kubeconfigPath
	cacheDir := t.TempDir()
	flags.CacheDir = &cacheDir
	helmClient := kube.New(flags)
	kube.ManagedFieldsManager = "helm-controller"
	build := func(object *unstructured.Unstructured) kube.ResourceList {
		payload, err := object.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		resources, err := helmClient.Build(bytes.NewReader(payload), true)
		if err != nil {
			t.Fatal(err)
		}
		return resources
	}
	update := func(before, target *unstructured.Unstructured, mode string) error {
		// The previous live release uses SSA, so auto inherits it. Use the
		// production controller's exact Helm SDK and rendered action settings.
		ssa := mode != "disabled"
		_, err := helmClient.Update(build(before), build(target),
			kube.ClientUpdateOptionServerSideApply(ssa, ssa),
			kube.ClientUpdateOptionForceReplace(false))
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	namespaces := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	_, err = namespaces.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "openbao"},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sets := client.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}).Namespace("openbao")
	for _, manager := range []string{"helm-controller", "legacy-controller"} {
		t.Run(manager, func(t *testing.T) {
			target := desired.DeepCopy()
			target.SetName("fixture-" + manager)
			before := target.DeepCopy()
			if err := unstructured.SetNestedMap(before.Object, map[string]any{
				"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": int64(2)},
			}, "spec", "updateStrategy"); err != nil {
				t.Fatal(err)
			}
			created, err := sets.Create(ctx, before, metav1.CreateOptions{FieldManager: manager})
			if err != nil {
				t.Fatal(err)
			}
			force := true // helm-controller (v1.5.5 through v1.6.5) forces conflicts for SSA upgrades.
			apply := func(object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				payload, err := object.MarshalJSON()
				if err != nil {
					t.Fatal(err)
				}
				return sets.Patch(ctx, object.GetName(), types.ApplyPatchType, payload,
					metav1.PatchOptions{FieldManager: "helm-controller", Force: &force})
			}
			// Characterize the production failure: omission leaves a retained field.
			omitted := target.DeepCopy()
			unstructured.RemoveNestedField(omitted.Object, "spec", "updateStrategy", "rollingUpdate")
			explicitNull := omitted.DeepCopy()
			if err := unstructured.SetNestedField(explicitNull.Object, nil, "spec", "updateStrategy", "rollingUpdate"); err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []*unstructured.Unstructured{omitted, explicitNull} {
				if _, err := apply(candidate); !apierrors.IsInvalid(err) ||
					!strings.Contains(err.Error(), "only allowed for updateStrategy 'RollingUpdate'") {
					t.Fatalf("SSA must reproduce the retained-field rejection, got %v", err)
				}
			}
			if err := update(before, target, release.Spec.Upgrade.ServerSideApply); err != nil {
				t.Fatalf("actual post-rendered transition rejected: %v", err)
			}
			after, err := sets.Get(ctx, target.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			strategy, _, err := unstructured.NestedMap(after.Object, "spec", "updateStrategy")
			if err != nil || len(strategy) != 1 || strategy["type"] != "OnDelete" {
				t.Fatalf("obsolete rolling settings retained: %#v (%v)", strategy, err)
			}
			if after.GetUID() != created.GetUID() {
				t.Fatal("strategy migration replaced the StatefulSet")
			}
			originalSpec, _, _ := unstructured.NestedMap(created.Object, "spec")
			afterSpec, _, _ := unstructured.NestedMap(after.Object, "spec")
			delete(originalSpec, "updateStrategy")
			delete(afterSpec, "updateStrategy")
			if !reflect.DeepEqual(originalSpec, afterSpec) {
				t.Fatal("strategy migration changed unrelated StatefulSet fields")
			}
			if err := update(target, before, release.Spec.Rollback.ServerSideApply); err != nil {
				t.Fatalf("configured Helm rollback rejected: %v", err)
			}
			rolledBack, err := sets.Get(ctx, target.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			partition, _, _ := unstructured.NestedInt64(rolledBack.Object, "spec", "updateStrategy", "rollingUpdate", "partition")
			if partition != 2 || rolledBack.GetUID() != created.GetUID() {
				t.Fatal("rollback lost the prior partition or replaced the StatefulSet")
			}
			if err := update(before, target, release.Spec.Upgrade.ServerSideApply); err != nil {
				t.Fatalf("repeat upgrade rejected: %v", err)
			}
			repeated, err := sets.Get(ctx, target.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			repeatedStrategy, _, _ := unstructured.NestedMap(repeated.Object, "spec", "updateStrategy")
			if len(repeatedStrategy) != 1 || repeatedStrategy["type"] != "OnDelete" || repeated.GetUID() != created.GetUID() {
				t.Fatal("repeat upgrade retained rolling settings or replaced the StatefulSet")
			}
		})
	}
}
