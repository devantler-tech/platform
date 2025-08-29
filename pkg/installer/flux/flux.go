package flux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
)

// Installer implements the installer interface for Flux.
type Installer struct {
	kubeconfig string
	context    string
	timeout    time.Duration
}

// New creates a new Flux installer.
func New(kubeconfig, context string, timeout time.Duration) *Installer {
	return &Installer{
		kubeconfig: kubeconfig,
		context:    context,
		timeout:    timeout,
	}
}

// Install installs or upgrades the Flux Operator via its OCI Helm chart.
func (i *Installer) Install() error {
	err := helmInstallOrUpgradeFluxOperator(i)
	if err != nil {
		return err
	}

	// TODO: Apply FluxInstance that syncs with local 'ksail-registry'
	return nil
}

// Uninstall removes the Helm release for the Flux Operator.
func (i *Installer) Uninstall() error {
	client, err := i.newHelmClient()
	if err != nil {
		return err
	}

	return client.UninstallReleaseByName("flux-operator")
}

// --- internals ---

func helmInstallOrUpgradeFluxOperator(i *Installer) error {
	client, err := i.newHelmClient()
	if err != nil {
		return err
	}

	spec := helmclient.ChartSpec{
		ReleaseName:     "flux-operator",
		ChartName:       "oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator",
		Namespace:       "flux-system",
		CreateNamespace: true,
		Atomic:          true,
		UpgradeCRDs:     true,
		Timeout:         i.timeout,
	}

	ctx, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()

	_, err = client.InstallOrUpgradeChart(ctx, &spec, nil)

	return err
}

func (i *Installer) newHelmClient() (helmclient.Client, error) {
	kubeconfigPath := expandPath(i.kubeconfig)

	data, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	opts := &helmclient.KubeConfClientOptions{
		Options: &helmclient.Options{
			Namespace: "flux-system",
		},
		KubeConfig:  data,
		KubeContext: i.context,
	}

	return helmclient.NewClientFromKubeConf(opts)
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