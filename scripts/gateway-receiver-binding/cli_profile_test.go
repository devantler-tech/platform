package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransportCLIRejectsMismatchedSourceBeforeAnyLiveOperation(t *testing.T) {
	if mode := os.Getenv("GATEWAY_TEST_ENTRYPOINT"); mode != "" {
		os.Args = []string{"gateway-receiver-binding", mode, "--context", "admin@prod"}
		main()
		return
	}
	dir := t.TempDir()
	infra := string(renderedResources(mutatingPolicy, validatingPolicy, mutatingBinding, validBinding)) + "---\napiVersion: gateway.networking.k8s.io/v1\nkind: Gateway\nmetadata:\n  name: platform\n  namespace: kube-system\nspec:\n  infrastructure:\n    annotations:\n      load-balancer.hetzner.cloud/uses-proxyprotocol: 'false'\n"
	controllers := "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: cilium\n  namespace: kube-system\nspec:\n  values:\n    gatewayAPI:\n      enableProxyProtocol: true\n"
	for name, content := range map[string]string{
		"infrastructure": infra,
		"controllers":    controllers,
		"kubectl":        "#!/bin/sh\ncase \"$*\" in\n  'kustomize k8s/providers/hetzner/infrastructure') cat \"$GATEWAY_TEST_DIR/infrastructure\";;\n  'kustomize k8s/providers/hetzner/infrastructure/controllers') cat \"$GATEWAY_TEST_DIR/controllers\";;\n  *) touch \"$GATEWAY_TEST_DIR/live-operation\"; exit 1;;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"restore-disabled", "apply-selected", "verify"} {
		command := exec.Command(executable, "-test.run=^TestTransportCLIRejectsMismatchedSourceBeforeAnyLiveOperation$")
		command.Env = append(os.Environ(), "GATEWAY_TEST_ENTRYPOINT="+mode, "GATEWAY_TEST_DIR="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "UNVERIFIED stage=transport-profile") {
			t.Errorf("%s did not reject the mismatched reviewed transport: %s", mode, output)
		}
		if _, err := os.Stat(filepath.Join(dir, "live-operation")); !os.IsNotExist(err) {
			t.Fatalf("%s reached a live operation before validating the paired source", mode)
		}
	}
}
