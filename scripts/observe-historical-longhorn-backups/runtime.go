package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/backupcatalogue"
	"github.com/devantler-tech/platform/internal/strictjson"
	"sigs.k8s.io/yaml"
)

func documentPaths(namespace string) []string {
	if namespace != "wedding-app" && namespace != "actual-budget" {
		return nil
	}
	return []string{
		"/apis/velero.io/v1/namespaces/velero/backupstoragelocations/default",
		"/apis/velero.io/v1/namespaces/velero/backuprepositories/" + namespace + "-default-kopia",
		"/api/v1/namespaces/velero/secrets/velero-r2-credentials",
		"/api/v1/namespaces/velero/secrets/velero-repo-credentials",
	}
}

func readAllowed(program string, args ...string) bool {
	switch program {
	case "git":
		return slices.Equal(args, []string{"ls-remote", "--exit-code", "https://github.com/devantler-tech/platform.git", "refs/heads/main"})
	case "kubectl":
		for _, namespace := range []string{"wedding-app", "actual-budget"} {
			for _, path := range documentPaths(namespace) {
				if slices.Equal(args, []string{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=" + path}) {
					return true
				}
			}
		}
	}
	return false
}

func readDocuments(ctx context.Context, execute command, namespace string) (backupcatalogue.DeploymentDocuments, error) {
	paths := documentPaths(namespace)
	if len(paths) != 4 || !active(ctx) {
		return backupcatalogue.DeploymentDocuments{}, errHeld
	}
	var docs backupcatalogue.DeploymentDocuments
	for i, destination := range []*[]byte{&docs.Location, &docs.Repository, &docs.CloudSecret, &docs.RepositorySecret} {
		if !active(ctx) {
			return backupcatalogue.DeploymentDocuments{}, errHeld
		}
		data, err := execute(ctx, "kubectl", "--context=admin@prod", "--request-timeout=20s", "get", "--raw="+paths[i])
		if err != nil || !active(ctx) || len(data) == 0 || len(data) > 512<<10 {
			return backupcatalogue.DeploymentDocuments{}, errHeld
		}
		*destination = data
	}
	return docs, nil
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
	held   bool
}

func (b *boundedOutput) Len() int { return b.buffer.Len() }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		b.held = true
		return 0, errHeld
	}
	return b.buffer.Write(data)
}

func executeRead(ctx context.Context, program string, args ...string) ([]byte, error) {
	if !active(ctx) || !readAllowed(program, args...) {
		return nil, errHeld
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, stderr := boundedOutput{limit: 512 << 10}, boundedOutput{limit: 64 << 10}
	process := exec.CommandContext(ctx, program, args...)
	process.Stdout, process.Stderr, process.WaitDelay = &stdout, &stderr, 2*time.Second
	process.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("LONGHORN_CATALOGUE_CREDENTIAL_DIR"), "KUBECONFIG=" + os.Getenv("KUBECONFIG"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if process.Run() != nil || !active(ctx) || stdout.held || stderr.held {
		return nil, errHeld
	}
	return stdout.buffer.Bytes(), nil
}

func requireCredentialFiles(get func(string) string) error {
	directory := get("LONGHORN_CATALOGUE_CREDENTIAL_DIR")
	if !regexp.MustCompile(`^/dev/shm/longhorn-catalogue\.[A-Za-z0-9]+$`).MatchString(directory) || get("KUBECONFIG") != filepath.Join(directory, "kubeconfig") {
		return errHeld
	}
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return errHeld
	}
	file, err := os.Lstat(get("KUBECONFIG"))
	if err != nil || !file.Mode().IsRegular() || file.Mode().Perm() != 0600 || file.Size() <= 0 || file.Size() > 512<<10 {
		return errHeld
	}
	return nil
}

func reviewedPolicy(ctx context.Context) (backupcatalogue.Policy, error) {
	values := map[string]string{}
	for _, layer := range []struct{ path, name string }{
		{"k8s/bases/bootstrap/config-map.yaml", "variables-base"},
		{"k8s/clusters/prod/bootstrap/config-map.yaml", "variables-cluster"},
	} {
		raw, err := os.ReadFile(layer.path)
		if err != nil || len(raw) == 0 || len(raw) > 64<<10 {
			return backupcatalogue.Policy{}, errHeld
		}
		data, err := yaml.YAMLToJSONStrict(raw)
		if err != nil || strictjson.Validate(data, map[string][]string{"": {"apiVersion", "kind", "metadata", "data"}, "metadata": {"name", "namespace"}}) != nil {
			return backupcatalogue.Policy{}, errHeld
		}
		var config struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct{ Name, Namespace string }
			Data       map[string]string
		}
		if yaml.UnmarshalStrict(raw, &config) != nil || config.APIVersion != "v1" || config.Kind != "ConfigMap" || config.Metadata.Name != layer.name || config.Metadata.Namespace != "flux-system" {
			return backupcatalogue.Policy{}, errHeld
		}
		for k, v := range config.Data {
			values[k] = v
		}
	}
	prefix := values["r2_prefix_velero"]
	if prefix == "" || strings.HasSuffix(prefix, "/") || values["r2_region"] != "auto" {
		return backupcatalogue.Policy{}, errHeld
	}
	policy := backupcatalogue.Policy{Endpoint: values["r2_endpoint"], Bucket: values["r2_bucket"], Prefix: prefix + "/", MaxRequests: 4096, MaxBytes: 512 << 20, MaxResponseBytes: 32 << 20}
	guard, err := backupcatalogue.NewReadTransport(ctx, policy, &http.Transport{})
	if err != nil {
		return backupcatalogue.Policy{}, errHeld
	}
	guard.Close()
	return policy, nil
}
