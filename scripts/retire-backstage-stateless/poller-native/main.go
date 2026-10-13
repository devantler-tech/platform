// This test-only driver exercises the published controller's exact native
// uninstall adapter and default SSA poller against a synthetic loopback API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"runtime/debug"
	"sync/atomic"
	"time"

	v2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/helm-controller/internal/action"
	"github.com/fluxcd/pkg/ssa"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var phase = "PINS"

func execute() error {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fmt.Errorf("native dependency identities unavailable")
	}
	pins := map[string]string{"github.com/fluxcd/helm-controller": "v1.5.5", "github.com/fluxcd/helm-controller/api": "v1.5.5", "github.com/fluxcd/pkg/ssa": "v0.67.6", "helm.sh/helm/v4": "v4.2.0"}
	for _, dep := range info.Deps {
		if v, required := pins[dep.Path]; required {
			if dep.Version != v || dep.Replace != nil {
				return fmt.Errorf("native dependency drift")
			}
			delete(pins, dep.Path)
		}
	}
	if len(pins) != 0 {
		return fmt.Errorf("missing native dependency identity")
	}
	phase = "ARGUMENTS"
	if len(os.Args) != 3 {
		return fmt.Errorf("invalid fixture arguments")
	}
	server, err := url.Parse(os.Args[1])
	if err != nil || server.Scheme != "http" || server.Path != "" || server.User != nil || !net.ParseIP(server.Hostname()).IsLoopback() {
		return fmt.Errorf("fixture requires loopback API")
	}
	var opts v2.Uninstall
	if json.Unmarshal([]byte(os.Args[2]), &opts) != nil || !opts.KeepHistory || !opts.DisableHooks || opts.DisableWait || opts.GetDeletionPropagation() != "foreground" || opts.Timeout == nil {
		return fmt.Errorf("invalid fixture controls")
	}
	getter := genericclioptions.NewConfigFlags(false)
	phase = "CLIENT"
	getter.APIServer = &os.Args[1]
	namespace := "backstage"
	getter.Namespace = &namespace
	// Explicit loopback-only configuration: never load ambient production auth.
	emptyConfig := ""
	getter.KubeConfig = &emptyConfig
	getter.WrapConfigFn = func(c *rest.Config) *rest.Config { return &rest.Config{Host: server.String(), QPS: 100, Burst: 100} }
	kclient, err := client.New(&rest.Config{Host: server.String(), QPS: 100, Burst: 100}, client.Options{})
	if err != nil {
		return err
	}
	var pollerCalls atomic.Int64
	phase = "FACTORY"
	factory, err := action.NewConfigFactory(getter, action.WithStorage(action.DefaultStorageDriver, namespace), action.WithResourceManager(func(...action.NewStatusReaderFunc) *ssa.ResourceManager {
		pollerCalls.Add(1)
		return ssa.NewResourceManager(kclient, nil, ssa.Owner{Field: "fixture", Group: "fixture.invalid"})
	}))
	if err != nil {
		return err
	}
	// Production has no explicit waitStrategy and no UseHelm3Defaults gate.
	if action.UseHelm3Defaults {
		return fmt.Errorf("unexpected native legacy defaults")
	}
	hr := &v2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Name: "backstage", Namespace: namespace}, Spec: v2.HelmReleaseSpec{Uninstall: &opts}}
	// Source prerequisite waiting does not gate the native deletion adapter.
	// The published controller dispatches deletion before source reconciliation.
	hr.Generation = 1
	hr.Status.ObservedGeneration = 1
	hr.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: "SourceNotReady", ObservedGeneration: 1},
		{Type: "Reconciling", Status: metav1.ConditionTrue, Reason: "Progressing", Message: "Fulfilling prerequisites", ObservedGeneration: 1},
		{Type: "Released", Status: metav1.ConditionTrue, Reason: "UpgradeSucceeded", ObservedGeneration: 1},
	}
	if hr.Spec.WaitStrategy != nil || hr.GetWaitStrategy() != "" {
		return fmt.Errorf("fixture changed native wait default")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	phase = "UNINSTALL"
	_, err = action.Uninstall(ctx, factory.Build(slog.NewTextHandler(io.Discard, nil)), hr, "backstage")
	if pollerCalls.Load() != 1 {
		return fmt.Errorf("production-equivalent native poller was not called")
	}
	fmt.Println("NATIVE_FLUX_POLLER=WAITED")
	phase = "WAITED"
	return err
}

func main() {
	if execute() != nil {
		if phase != "WAITED" {
			fmt.Println("NATIVE_FLUX_POLLER=HOLD_" + phase)
		}
		os.Exit(1)
	}
}
