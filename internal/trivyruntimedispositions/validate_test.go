package trivyruntimedispositions

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repository(t *testing.T) string {
	t.Helper()
	p, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func write(t *testing.T, root, path string, data []byte) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	paths := []string{".trivyignore.yaml"}
	for p := range sourcePins {
		paths = append(paths, p)
	}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(repository(t), p))
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, p, data)
	}
	if err := validateSource(root); err != nil {
		t.Fatalf("baseline fixture invalid: %v", err)
	}
	return root
}

func replace(t *testing.T, root, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("ineffective mutation: %s / %q", path, old)
	}
	write(t, root, path, []byte(strings.Replace(string(data), old, new, 1)))
}

func TestChangedRuntimePremisesRefuse(t *testing.T) {
	cases := []struct{ name, path, old, new string }{
		{"node-image", "k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile", "ghcr.io/coroot/coroot-node-agent:1.38.1", "unreviewed.example/node-agent:1.38.1"},
		{"dummy-healthcheck", "k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile", "COPY --chmod", "HEALTHCHECK NONE\nCOPY --chmod"},
		{"publisher-source", ".github/workflows/publish-coroot-node-agent-hotfix.yaml", "repository: coroot/coroot-node-agent", "repository: other/node-agent"},
		{"mail-uid", "k8s/bases/apps/private-mail/stateful-set.yaml", "runAsUser: 2000", "runAsUser: 2001"},
		{"mail-gid", "k8s/bases/apps/private-mail/stateful-set.yaml", "runAsGroup: 2000", "runAsGroup: 2001"},
		{"mail-capability", "k8s/bases/apps/private-mail/stateful-set.yaml", "add: [NET_BIND_SERVICE]", "add: [NET_BIND_SERVICE, SYS_ADMIN]"},
		{"mail-secret-key", "k8s/bases/apps/private-mail/stateful-set.yaml", "key: credential", "key: password"},
		{"mail-image", "k8s/bases/apps/private-mail/stateful-set.yaml", "stalwart:v0.16.27", "stalwart:v0.16.28"},
		{"mail-extra-container", "k8s/bases/apps/private-mail/stateful-set.yaml", "containers:", "containers:\n        - name: additional\n          image: registry.k8s.io/pause:3.10"},
		{"mail-new-secret-env", "k8s/bases/apps/private-mail/stateful-set.yaml", "- name: STALWART_RECOVERY_MODE", "- name: EXTRA_SECRET\n              valueFrom:\n                secretKeyRef: {name: extra, key: password}\n            - name: STALWART_RECOVERY_MODE"},
		{"collector-write", "k8s/bases/infrastructure/coroot/components/security-progress/cluster-role.yaml", "verbs: [get, list]", "verbs: [get, list, patch]"},
		{"collector-wildcard", "k8s/bases/infrastructure/coroot/components/security-progress/cluster-role.yaml", "verbs: [get, list]", "verbs: ['*']"},
		{"collector-token-ttl", "k8s/bases/infrastructure/coroot/components/security-progress/deployment.yaml", "expirationSeconds: 3600", "expirationSeconds: 86400"},
		{"vault-token-ttl", "k8s/bases/infrastructure/vault-backup/cron-job.yaml", "expirationSeconds: 3600", "expirationSeconds: 86400"},
		{"vault-automount", "k8s/bases/infrastructure/vault-backup/service-account.yaml", "automountServiceAccountToken: false", "automountServiceAccountToken: true"},
		{"helm-chart", "k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml", "version: 3.9.2", "version: 3.9.3"},
		{"scope-glob", ".trivyignore.yaml", "k8s/bases/apps/private-mail/stateful-set.yaml", "k8s/bases/apps/private-mail/*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := fixture(t)
			replace(t, root, c.path, c.old, c.new)
			if err := validateSource(root); err == nil {
				t.Fatal("changed runtime premise retained disposition")
			}
		})
	}
}

func TestPublicGuardConnectsSourcePremiseBeforeNativeRendering(t *testing.T) {
	root := fixture(t)
	path := "k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile"
	replace(t, root, path, "ghcr.io/coroot/coroot-node-agent:1.38.1", "unreviewed.example/agent:1.38.1")
	err := Validate(root)
	if err == nil || !strings.Contains(err.Error(), path+": runtime premise changed") {
		t.Fatalf("public guard did not refuse the actual source premise before any native render: %v", err)
	}
}

func TestChangedRBACPremisesRefuse(t *testing.T) {
	cases := []struct{ name, path, old, new string }{
		{"detector-write", "k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role.yaml", "- patch", "- delete"},
		{"detector-name", "k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role.yaml", "- prune-protected-orphan-alert-state", "- another-state"},
		{"detector-subject", "k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role-binding.yaml", "kind: ServiceAccount", "kind: Group"},
		{"detector-consumer", "k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/cron-job.yaml", "STATE_PATH=/api/v1/namespaces/observability/configmaps/prune-protected-orphan-alert-state", "STATE_PATH=/api/v1/namespaces/observability/secrets/extra"},
		{"longhorn-wildcard", "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-csi-sidecars.yaml", "- csi-attacher", "- '*'"},
		{"longhorn-namespace", "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-csi-plugin.yaml", "namespace: longhorn-system", "namespace: default"},
		{"longhorn-binding", "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-binding-kyverno-mutate-longhorn-engine-image.yaml", "namespace: kyverno", "namespace: other"},
		{"longhorn-target", "k8s/providers/hetzner/infrastructure/cluster-policies/add-existing-csi-sidecar-baseline-context.yaml", "csi-attacher", "longhorn-manager"},
		{"mandatory-native-test", "scripts/tests/test-longhorn-engine-image-baseline-context.sh", "kyverno apply", "true # kyverno apply"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := fixture(t)
			replace(t, root, c.path, c.old, c.new)
			if err := validateSource(root); err == nil {
				t.Fatal("changed RBAC premise accepted")
			}
		})
	}
}
func TestNovelRBACConsumersRefuse(t *testing.T) {
	cases := map[string]string{
		"kyverno-consumer":   "apiVersion: v1\nkind: Pod\nmetadata: {name: extra, namespace: kyverno}\nspec: {serviceAccountName: kyverno-background-controller, containers: []}\n",
		"role-binding":       "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: extra}\nroleRef: {kind: Role, name: 'kyverno:background-controller:mutate-csi-sidecars'}\nsubjects: [{kind: User, name: another}]\n",
		"controller-binding": "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: extra}\nroleRef: {kind: ClusterRole, name: admin}\nsubjects: [{kind: ServiceAccount, name: kyverno-background-controller, namespace: kyverno}]\n",
		"detector-binding":   "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: extra}\nroleRef: {kind: Role, name: prune-protected-orphan-alert}\nsubjects: [{kind: User, name: another}]\n",
		"detector-consumer":  "apiVersion: v1\nkind: Pod\nmetadata: {name: extra}\nspec: {serviceAccountName: prune-protected-orphan-alert, containers: []}\n",
		"static-policy":      "apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata: {name: extra}\nspec:\n  rules:\n    - name: extra\n      mutate:\n        targets: [{apiVersion: apps/v1, kind: Deployment, namespace: longhorn-system, name: csi-attacher}]\n",
		"wildcard-policy":    "apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata: {name: extra}\nspec:\n  rules:\n    - name: extra\n      mutate:\n        targets: [{apiVersion: apps/v1, kind: DaemonSet, namespace: '*', name: '*'}]\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			root := fixture(t)
			write(t, root, "k8s/new-rbac-consumer.yaml", []byte(data))
			if err := validateSource(root); err == nil {
				t.Fatal("novel RBAC consumer accepted")
			}
			if err := checkRendered("test", []byte(data), map[string]string{}); err == nil {
				t.Fatal("novel effective RBAC consumer accepted")
			}
		})
	}
}

func TestNovelConsumersAndBindingsRefuse(t *testing.T) {
	for name, data := range map[string]string{
		"unmarked-annotation": "apiVersion: v1\nkind: Pod\nmetadata: {name: extra, annotations: {checkov.io/skip1: 'CKV_K8S_25=unreviewed exception'}}\n",
		"consumer":            "apiVersion: v1\nkind: Pod\nmetadata: {name: extra}\nspec: {serviceAccountName: vault-snapshot, containers: []}\n",
		"factory":             "apiVersion: v1\nkind: Pod\nmetadata: {name: extra}\nspec:\n  containers: [{name: extra, image: ghcr.io/devantler-tech/platform-coroot:unreviewed}]\n",
		"binding":             "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: extra}\nroleRef: {kind: Role, name: extra}\nsubjects: [{kind: ServiceAccount, name: coroot-security-progress, namespace: observability}]\n",
		"role-consumer":       "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: extra}\nroleRef: {kind: ClusterRole, name: coroot-security-progress}\nsubjects: [{kind: ServiceAccount, name: other, namespace: observability}]\n",
		"broad-binding":       "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: extra}\nroleRef: {kind: ClusterRole, name: admin}\nsubjects: [{kind: Group, name: 'system:serviceaccounts'}]\n",
		"list-binding":        "apiVersion: v1\nkind: List\nitems:\n- apiVersion: rbac.authorization.k8s.io/v1\n  kind: RoleBinding\n  metadata: {name: extra}\n  roleRef: {kind: Role, name: extra}\n  subjects: [{kind: ServiceAccount, name: vault-snapshot, namespace: openbao}]\n",
		"annotation":          "apiVersion: v1\nkind: Pod\nmetadata: {name: extra, annotations: {checkov.io/skip1: 'CKV_K8S_25=[runtime-premise] copied'}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := fixture(t)
			write(t, root, "k8s/new-consumer.yaml", []byte(data))
			if err := validateSource(root); err == nil {
				t.Fatal("new consumer/binding retained disposition")
			}
		})
	}
}

func TestUnrelatedAndSerializationChangesPass(t *testing.T) {
	root := fixture(t)
	write(t, root, "k8s/unrelated.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: unrelated}\ndata: {value: changed}\n"))
	path := "k8s/bases/apps/private-mail/stateful-set.yaml"
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := object(data)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, path, serialized)
	if err := validateSource(root); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSourceIdentityRefuses(t *testing.T) {
	path := "k8s/bases/apps/private-mail/stateful-set.yaml"
	for _, mode := range []string{"missing", "unreadable", "linked", "duplicate", "multidoc"} {
		t.Run(mode, func(t *testing.T) {
			root := fixture(t)
			full := filepath.Join(root, path)
			switch mode {
			case "missing":
				if err := os.Remove(full); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Chmod(full, 0000); err != nil {
					t.Fatal(err)
				}
			case "linked":
				data, err := os.ReadFile(full)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "external.yaml")
				if err = os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(full); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, full); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				replace(t, root, path, "kind: StatefulSet", "kind: StatefulSet\nkind: StatefulSet")
			case "multidoc":
				data, err := os.ReadFile(full)
				if err != nil {
					t.Fatal(err)
				}
				write(t, root, path, append(data, []byte("\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: hidden}\n")...))
			}
			if err := validateSource(root); err == nil {
				t.Fatal("invalid source retained disposition")
			}
		})
	}
}

func TestEffectiveBoundary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repository(t), "k8s/bases/apps/private-mail/stateful-set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := object(data)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{objectKey(obj): fmt.Sprintf("%x", sha256.Sum256(canonical))}
	unrelated := []byte("\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: unrelated}\ndata: {anything: changed}\n")
	if err := checkRendered("test", append(append([]byte{}, data...), unrelated...), expected); err != nil {
		t.Fatal(err)
	}
	if err := checkRendered("test", canonical, expected); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string][]byte{
		"mail-uid":    []byte(strings.Replace(string(data), "runAsUser: 2000", "runAsUser: 2001", 1)),
		"duplicate":   append(append(append([]byte{}, data...), []byte("\n---\n")...), data...),
		"missing":     unrelated[5:],
		"new-binding": append(append([]byte{}, data...), []byte("\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: extra}\nroleRef: {kind: Role, name: any}\nsubjects: [{kind: ServiceAccount, name: vault-snapshot, namespace: openbao}]\n")...),
		"new-factory": append(append([]byte{}, data...), []byte("\n---\napiVersion: v1\nkind: Pod\nmetadata: {name: extra}\nspec:\n  containers: [{name: extra, image: ghcr.io/devantler-tech/platform-coroot:unreviewed}]\n")...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkRendered("test", output, expected); err == nil {
				t.Fatal("effective changed premise accepted")
			}
		})
	}
}
