package kubectl

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

//go:embed assets/kubectl/apply-set-crd.yaml
var applySetCRDYAML []byte

//go:embed assets/kubectl/apply-set-cr.yaml
var applySetCRYAML []byte

// Installer implements the installer interface for kubectl.
type Installer struct {
	kubeconfig string
	context    string
	timeout    time.Duration
}

// New creates a new kubectl installer.
func New(kubeconfig, context string, timeout time.Duration) *Installer {
	return &Installer{
		kubeconfig: kubeconfig,
		context:    context,
		timeout:    timeout,
	}
}

// Install ensures the ApplySet CRD and its parent CR exist.
func (i *Installer) Install() error {
	restConfigWrapper, err := i.buildRESTConfig()
	if err != nil {
		return err
	}

	// --- CRD ---
	apiExtClient, err := apiextensionsclient.NewForConfig(restConfigWrapper)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	context, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()

	const crdName = "applysets.k8s.devantler.tech"

	_, err = apiExtClient.ApiextensionsV1().CustomResourceDefinitions().Get(context, crdName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		fmt.Println("► applying applysets crd 'applysets.k8s.devantler.tech'")

		err := i.applyCRD(context, apiExtClient)
		if err != nil {
			return err
		}

		err = i.waitForCRDEstablished(context, apiExtClient, crdName)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	fmt.Println("✔ applysets crd 'applysets.k8s.devantler.tech' applied")

	// --- CR (ApplySet parent) ---
	dynClient, err := dynamic.NewForConfig(restConfigWrapper)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{Group: "k8s.devantler.tech", Version: "v1", Resource: "applysets"}

	const applySetName = "ksail"

	_, err = dynClient.Resource(gvr).Get(context, applySetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		fmt.Println("► applying applysets cr 'ksail'")

		err := i.applyApplySetCR(context, dynClient, gvr, applySetName)
		if err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("failed to get ApplySet CR: %w", err)
	}

	fmt.Println("✔ applysets cr 'ksail' applied")

	return nil
}

// Uninstall deletes the ApplySet CR then its CRD.
func (i *Installer) Uninstall() error {
	config, err := i.buildRESTConfig()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{Group: "k8s.devantler.tech", Version: "v1", Resource: "applysets"}
	_ = dynClient.Resource(gvr).Delete(ctx, "ksail", metav1.DeleteOptions{}) // ignore errors (including NotFound)

	apiExtClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	_ = apiExtClient.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, "applysets.k8s.devantler.tech", metav1.DeleteOptions{})

	return nil
}

// --- internals ---

func (i *Installer) buildRESTConfig() (*rest.Config, error) {
	kubeconfigPath := expandPath(i.kubeconfig)
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}

	overrides := &clientcmd.ConfigOverrides{}
	if i.context != "" {
		overrides.CurrentContext = i.context
	}

	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	restConfig, err := clientCfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build rest config: %w", err)
	}

	return restConfig, nil
}

// applyCRD creates the ApplySet CRD from embedded YAML.
func (i *Installer) applyCRD(ctx context.Context, c *apiextensionsclient.Clientset) error {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(applySetCRDYAML, &crd); err != nil {
		return fmt.Errorf("failed to unmarshal CRD yaml: %w", err)
	}
	// Attempt create; if already exists attempt update (could race).
	_, err := c.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := c.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to get existing CRD for update: %w", getErr)
		}

		crd.ResourceVersion = existing.ResourceVersion
		if _, uerr := c.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, &crd, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("failed to update CRD: %w", uerr)
		}

		return nil
	}

	return err
}

func (i *Installer) waitForCRDEstablished(ctx context.Context, c *apiextensionsclient.Clientset, name string) error {
	// Poll every 500ms until Established=True or timeout
	pollCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(pollCtx, 500*time.Millisecond, i.timeout, true, func(ctx context.Context) (bool, error) {
		crd, err := c.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}

		for _, cond := range crd.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}

			if cond.Type == apiextensionsv1.NamesAccepted && cond.Status == apiextensionsv1.ConditionFalse && cond.Reason == "MultipleNamesNotAllowed" {
				return false, errors.New(cond.Message)
			}
		}

		return false, nil
	})
}

func (i *Installer) applyApplySetCR(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, name string) error {
	var u unstructured.Unstructured
	if err := yaml.Unmarshal(applySetCRYAML, &u.Object); err != nil {
		return fmt.Errorf("failed to unmarshal ApplySet CR yaml: %w", err)
	}
	// Ensure GVK since yaml->map won't set it.
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.devantler.tech", Version: "v1", Kind: "ApplySet"})
	u.SetName(name)

	_, err := dyn.Resource(gvr).Create(ctx, &u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to get existing ApplySet: %w", getErr)
		}

		u.SetResourceVersion(existing.GetResourceVersion())

		if _, uerr := dyn.Resource(gvr).Update(ctx, &u, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("failed to update ApplySet: %w", uerr)
		}

		return nil
	}

	return err
}

// expandPath expands the ~ character in file paths to the user's home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if homeDir, err := os.UserHomeDir(); err == nil {
			return filepath.Join(homeDir, path[2:])
		}
	}
	return path
}