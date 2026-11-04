package trivyruntimedispositions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Only these normalized relevant objects are fingerprinted, never whole layers.
// Unrelated objects may change. Both provider overlays must preserve the audited
// runtime premises, and new consumers/bindings of these identities fail closed.
var effectiveKeys = map[string]bool{
	"apps/v1/Deployment/observability/coroot-security-progress":                                                           true,
	"apps/v1/StatefulSet/private-mail/private-mail":                                                                       true,
	"batch/v1/CronJob/observability/prune-protected-orphan-alert":                                                         true,
	"batch/v1/CronJob/openbao/vault-snapshot":                                                                             true,
	"batch/v1/Job/openbao/vault-config":                                                                                   true,
	"batch/v1/Job/openbao/vault-snapshot-init":                                                                            true,
	"cert-manager.io/v1/Certificate/world-at-ruin/zone-trial":                                                             true,
	"coroot.com/v1/Coroot/observability/coroot":                                                                           true,
	"external-secrets.io/v1/ExternalSecret/private-mail/private-mail-admin":                                               true,
	"external-secrets.io/v1/ExternalSecret/private-mail/private-mail-generated-admin":                                     true,
	"external-secrets.io/v1/SecretStore/world-at-ruin/world-at-ruin":                                                      true,
	"external-secrets.io/v1alpha1/PushSecret/private-mail/private-mail-admin":                                             true,
	"generators.external-secrets.io/v1alpha1/Password/private-mail/private-mail-admin":                                    true,
	"helm.toolkit.fluxcd.io/v2/HelmRelease/kyverno/kyverno":                                                               true,
	"helm.toolkit.fluxcd.io/v2/HelmRelease/longhorn-system/longhorn":                                                      true,
	"helm.toolkit.fluxcd.io/v2/HelmRelease/observability/coroot-operator":                                                 true,
	"kubescape.io/v1beta1/ClusterSecurityException//world-at-ruin-trial-operator":                                         true,
	"kustomize.toolkit.fluxcd.io/v1/Kustomization/world-at-ruin/world-at-ruin":                                            true,
	"kyverno.io/v1/ClusterPolicy//add-existing-csi-sidecar-baseline-context":                                              true,
	"kyverno.io/v1/ClusterPolicy//add-existing-longhorn-csi-plugin-baseline-context":                                      true,
	"kyverno.io/v1/ClusterPolicy//add-existing-longhorn-engine-image-baseline-context":                                    true,
	"kyverno.io/v1/ClusterPolicy//remove-existing-longhorn-registrar-prestop":                                             true,
	"kyverno.io/v1/ClusterPolicy//remove-longhorn-registrar-prestop":                                                      true,
	"networking.k8s.io/v1/NetworkPolicy/world-at-ruin/default-deny":                                                       true,
	"rbac.authorization.k8s.io/v1/ClusterRole//coroot-security-progress":                                                  true,
	"rbac.authorization.k8s.io/v1/ClusterRole//prune-protected-orphan-alert":                                              true,
	"rbac.authorization.k8s.io/v1/ClusterRoleBinding//coroot-security-progress":                                           true,
	"rbac.authorization.k8s.io/v1/ClusterRoleBinding//prune-protected-orphan-alert":                                       true,
	"rbac.authorization.k8s.io/v1/Role/longhorn-system/kyverno:background-controller:mutate-csi-sidecars":                 true,
	"rbac.authorization.k8s.io/v1/Role/longhorn-system/kyverno:background-controller:mutate-longhorn-csi-plugin":          true,
	"rbac.authorization.k8s.io/v1/Role/longhorn-system/kyverno:background-controller:mutate-longhorn-engine-image":        true,
	"rbac.authorization.k8s.io/v1/Role/observability/prune-protected-orphan-alert":                                        true,
	"rbac.authorization.k8s.io/v1/Role/world-at-ruin/world-at-ruin-zone-trial-operator":                                   true,
	"rbac.authorization.k8s.io/v1/RoleBinding/longhorn-system/kyverno:background-controller:mutate-csi-sidecars":          true,
	"rbac.authorization.k8s.io/v1/RoleBinding/longhorn-system/kyverno:background-controller:mutate-longhorn-csi-plugin":   true,
	"rbac.authorization.k8s.io/v1/RoleBinding/longhorn-system/kyverno:background-controller:mutate-longhorn-engine-image": true,
	"rbac.authorization.k8s.io/v1/RoleBinding/observability/prune-protected-orphan-alert":                                 true,
	"rbac.authorization.k8s.io/v1/RoleBinding/umami/kyverno:background-controller:mutate-umami-primary":                   true,
	"rbac.authorization.k8s.io/v1/RoleBinding/world-at-ruin/world-at-ruin":                                                true,
	"rbac.authorization.k8s.io/v1/RoleBinding/world-at-ruin/world-at-ruin-zone-trial-operator":                            true,
	"source.toolkit.fluxcd.io/v1/OCIRepository/world-at-ruin/world-at-ruin":                                               true,
	"v1/ConfigMap/observability/coroot-security-progress":                                                                 true,
	"v1/ConfigMap/observability/prune-protected-orphan-alert-state":                                                       true,
	"v1/LimitRange/world-at-ruin/zone-trial":                                                                              true,
	"v1/Namespace//world-at-ruin":                                                                                         true,
	"v1/ResourceQuota/world-at-ruin/zone-trial":                                                                           true,
	"v1/ServiceAccount/observability/coroot-security-progress":                                                            true,
	"v1/ServiceAccount/observability/prune-protected-orphan-alert":                                                        true,
	"v1/ServiceAccount/openbao/vault-snapshot":                                                                            true,
	"v1/ServiceAccount/private-mail/private-mail":                                                                         true,
	"v1/ServiceAccount/world-at-ruin/world-at-ruin":                                                                       true,
}
var effectivePins = map[string]map[string]string{
	"docker/apps": {},
	"docker/infrastructure": {
		"batch/v1/Job/openbao/vault-config": "89960be5d0ac22a65248397fbd9eee5ebf0eed4c7a0c4c5fe5b366d5d857e374",
	},
	"docker/infrastructure/controllers": {
		"helm.toolkit.fluxcd.io/v2/HelmRelease/kyverno/kyverno":                                             "6c1f87f822a18d33818848e080aec19af9bd5cdf6b9b94d4afdd2da1c3f00ad8",
		"rbac.authorization.k8s.io/v1/RoleBinding/umami/kyverno:background-controller:mutate-umami-primary": "8291bce476ab9a173840a4448d8bb7c86f9fa967f671e5d8d409c69f558074dc",
	},
	"hetzner/apps": {
		"apps/v1/StatefulSet/private-mail/private-mail":                                            "c2eb339f3cf960eeac4e8506b1ca1060ae9d9497040668a0e50a69aecbfd14aa",
		"cert-manager.io/v1/Certificate/world-at-ruin/zone-trial":                                  "41cdd7bc7407d8de59f83172c8d471dc2b04af158a73ba1d17cd275ee032f6e2",
		"external-secrets.io/v1/ExternalSecret/private-mail/private-mail-admin":                    "2e1659cbf9bfff043a364e35124fe6e3e578baed1f8d7537b9e09271235c0f89",
		"external-secrets.io/v1/ExternalSecret/private-mail/private-mail-generated-admin":          "549d62c92df3307eb54d535b3b103ad9f19233c8d286d3cb723524213641410f",
		"external-secrets.io/v1/SecretStore/world-at-ruin/world-at-ruin":                           "4bea9858e894928e6904d96dbaf75ef9e7bc44da86be3a347f41ddaa1643a878",
		"generators.external-secrets.io/v1alpha1/Password/private-mail/private-mail-admin":         "3c6b0f0f3463b9852b6fa85b799b11572c38ee338e64506544543aac907de0ea",
		"kustomize.toolkit.fluxcd.io/v1/Kustomization/world-at-ruin/world-at-ruin":                 "6d5aa535c3ba81b590b1bba38f78a1cee49321f35ff9e4798f59247ed2bb40bd",
		"networking.k8s.io/v1/NetworkPolicy/world-at-ruin/default-deny":                            "e75019d15d0e0d8996b336b45047df385fc16bf217dbaee548c8f2010ed0f885",
		"rbac.authorization.k8s.io/v1/Role/world-at-ruin/world-at-ruin-zone-trial-operator":        "24d8f8ad2fed9d34edfd226c9de70f6b887db633155e092cd0a39d0fb04eac3b",
		"rbac.authorization.k8s.io/v1/RoleBinding/world-at-ruin/world-at-ruin":                     "7a2c282945cddb675cc5bb2d8083b0b589a7e7cccaeea366fe1f13ff3e75e541",
		"rbac.authorization.k8s.io/v1/RoleBinding/world-at-ruin/world-at-ruin-zone-trial-operator": "ca40a63a7459a4e1e68b6b9fc5df9b85cd8811487fb1844c648c289e3f0cca8b",
		"source.toolkit.fluxcd.io/v1/OCIRepository/world-at-ruin/world-at-ruin":                    "fd6e200394ce4cf71a06003cb477d41e0d15d0b0284c069af1bf29b9673be403",
		"v1/LimitRange/world-at-ruin/zone-trial":                                                   "105036a0563e542e6dbe5ed4c8ac0b37e26199558e832ff61162549f4d7085b5",
		"v1/Namespace//world-at-ruin":                                                              "5642413ecb023e6e179b584798b7bf3014f59328d689e3c9701ab04480f1cb1b",
		"v1/ResourceQuota/world-at-ruin/zone-trial":                                                "7d28b72227d7ad97d2a2c73aa1799676b84727988d2636f494a999eea80e5926",
		"v1/ServiceAccount/private-mail/private-mail":                                              "02c56efad279dbfed0954493d96b7701040bab224f411e2ac98f9b900ecd12bc",
		"v1/ServiceAccount/world-at-ruin/world-at-ruin":                                            "c9ef76aadb0fe80f6983d54993ab5fcb6f003db84451bd65f687e55b302a2cb8",
	},
	"hetzner/infrastructure": {
		"apps/v1/Deployment/observability/coroot-security-progress":                        "a75568ab0c4539b4eb713ad5a4788e6b89fd732a0e9b718b9ebf5ae291a1d4d3",
		"batch/v1/CronJob/openbao/vault-snapshot":                                          "b43b9a3f826ec8a4581b000302b8afa299d9ea28dd7202f6d243ac1aeb04bd29",
		"batch/v1/Job/openbao/vault-config":                                                "89960be5d0ac22a65248397fbd9eee5ebf0eed4c7a0c4c5fe5b366d5d857e374",
		"batch/v1/Job/openbao/vault-snapshot-init":                                         "318e5ce6dbff51b3a115774094025f22b82b02391b52f1f274c189f1fa93ef21",
		"coroot.com/v1/Coroot/observability/coroot":                                        "ea5afd93c23d8359f2dbf43028eb9b1d23b282afe67205773a929412d0e37190",
		"kubescape.io/v1beta1/ClusterSecurityException//world-at-ruin-trial-operator":      "d2503616861fee13e7a8ac92c715d5ce3dc43b9c54a0bba6baaca60603d9857e",
		"kyverno.io/v1/ClusterPolicy//add-existing-csi-sidecar-baseline-context":           "f61d490f95bc4677557dfc854f786a01f6617df30c15210807df9229514b29e9",
		"kyverno.io/v1/ClusterPolicy//add-existing-longhorn-csi-plugin-baseline-context":   "c579b175eb0c259c26b68c77521c79c82d217cb9b58c182269026b2254c35a35",
		"kyverno.io/v1/ClusterPolicy//add-existing-longhorn-engine-image-baseline-context": "cdabe031dab1917504a946756e14af61377eb025509464d0c873692b5fd5252e",
		"kyverno.io/v1/ClusterPolicy//remove-existing-longhorn-registrar-prestop":          "20a75e937dd4d08f2504ba92851796c8f474f3772dd9ad8c9d6cd9409e731694",
		"kyverno.io/v1/ClusterPolicy//remove-longhorn-registrar-prestop":                   "ec6132e525a3ae177fa969642f37f7c55ef65b2a43dc97822fef34451d421129",
		"rbac.authorization.k8s.io/v1/ClusterRole//coroot-security-progress":               "fc60e73f00df723fbbfcf388d02e8780f8c5196e6fa695c5494aa20d69fb3d1b",
		"rbac.authorization.k8s.io/v1/ClusterRoleBinding//coroot-security-progress":        "2387f5266c89981d89f952f1f6b46d93188500dd99aeedd8b4170bbfa072ae2b",
		"v1/ConfigMap/observability/coroot-security-progress":                              "8a497be7097f2508814f67b42cc94440d823f52632ff08d9e0d63decc12995b5",
		"v1/ServiceAccount/observability/coroot-security-progress":                         "703cd6e16da10a69e2d97b5935bc7b3d54dd59e241e80697075fd447e4f32957",
		"v1/ServiceAccount/openbao/vault-snapshot":                                         "9b1513bb176dc77d4f1bc838e5efc7a7ec91173aa30c954db9f7eba7fe96e4f7",
	},
	"hetzner/infrastructure/controllers": {
		"batch/v1/CronJob/observability/prune-protected-orphan-alert":                                                         "45198991b275a56c109bdab186e2d7402101b440b23ce98504e490f36928e44c",
		"helm.toolkit.fluxcd.io/v2/HelmRelease/kyverno/kyverno":                                                               "db1d44c89402bc4cee6b38782d7d589139420485f26f44fed0fb72758f8edc9a",
		"helm.toolkit.fluxcd.io/v2/HelmRelease/longhorn-system/longhorn":                                                      "5a69c1e7aa5dc507ce50b6092a1024f0e41c5ced04027d6429b126c4f6b93bcf",
		"helm.toolkit.fluxcd.io/v2/HelmRelease/observability/coroot-operator":                                                 "e897e8eab73e4bd27a7a6942b76c884e37a80b9b25eb048b300a6d33f81898f8",
		"rbac.authorization.k8s.io/v1/ClusterRole//prune-protected-orphan-alert":                                              "7c20ba75729481e8f4ee345e52c86cdbf6922867e19b64d2b585d20c0b3aad40",
		"rbac.authorization.k8s.io/v1/ClusterRoleBinding//prune-protected-orphan-alert":                                       "7450a9a9b9cdd9bfa623bdb7075a77dfb6f56810b4101554b326c6c4c2b32216",
		"rbac.authorization.k8s.io/v1/Role/longhorn-system/kyverno:background-controller:mutate-csi-sidecars":                 "eacf2fecdcb86e7aeaec65f3e4aa343519e313538e86ce5f00687a0f3e5804a2",
		"rbac.authorization.k8s.io/v1/Role/longhorn-system/kyverno:background-controller:mutate-longhorn-csi-plugin":          "24b8879d119c3a773eeab6e92fc121f884928fe39db24b63e9d32ecdde150125",
		"rbac.authorization.k8s.io/v1/Role/longhorn-system/kyverno:background-controller:mutate-longhorn-engine-image":        "2be24eea591c991dbc39e6bd5f2700b9985f43c6f8761880480b36cdd94c9474",
		"rbac.authorization.k8s.io/v1/Role/observability/prune-protected-orphan-alert":                                        "785d69eaa42bb6a8886b80e23730c71f7027f40cd07496d0beae7eff388ecd6f",
		"rbac.authorization.k8s.io/v1/RoleBinding/longhorn-system/kyverno:background-controller:mutate-csi-sidecars":          "431a227f7b1c8e9298ee87b3c2d152e15944a17dec25189bdc62871509fbb1de",
		"rbac.authorization.k8s.io/v1/RoleBinding/longhorn-system/kyverno:background-controller:mutate-longhorn-csi-plugin":   "39d56217018780a69d52d757eb9d7f9a1ca1030863d5441b00772f138d6e87b0",
		"rbac.authorization.k8s.io/v1/RoleBinding/longhorn-system/kyverno:background-controller:mutate-longhorn-engine-image": "f812392b5ed288eb71b054c347e9bc29cf73583c42d903c15c9f963f275e8de4",
		"rbac.authorization.k8s.io/v1/RoleBinding/observability/prune-protected-orphan-alert":                                 "e7195bbc7b202f64d4486964f8c0b9a1b4b2270e45dd491bbdb2f1626a53c891",
		"rbac.authorization.k8s.io/v1/RoleBinding/umami/kyverno:background-controller:mutate-umami-primary":                   "8291bce476ab9a173840a4448d8bb7c86f9fa967f671e5d8d409c69f558074dc",
		"v1/ConfigMap/observability/prune-protected-orphan-alert-state":                                                       "2fd788c6dc1fb8c2cb0e078b14f00852ec5422a452ea22e014d337831a253aaa",
		"v1/ServiceAccount/observability/prune-protected-orphan-alert":                                                        "f0a74c5b30121990ece28557fe3f0f7ccf5ba65530dbde08a317eef4456b3b54",
	},
}

func objectKey(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	api, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	name, _ := meta["name"].(string)
	namespace, _ := meta["namespace"].(string)
	if api == "" || kind == "" || name == "" {
		return ""
	}
	return api + "/" + kind + "/" + namespace + "/" + name
}

func checkEffective(root string) error {
	for layer, expected := range effectivePins {
		if err := localKustomize(root, "k8s/providers/"+layer, map[string]bool{}); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		cmd := exec.CommandContext(ctx, "kubectl", "kustomize", "k8s/providers/"+layer)
		cmd.Dir = root
		// This command reads local Kustomize inputs and never contacts a cluster.
		cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		cancel()
		if err != nil {
			return fmt.Errorf("%s: native offline Kustomize failed: %w", layer, err)
		}
		if err = checkRendered(layer, output, expected); err != nil {
			return err
		}
	}
	return nil
}

func checkRendered(layer string, output []byte, expected map[string]string) error {
	actual := map[string]string{}
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	for {
		var obj map[string]any
		if err := decoder.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("%s: invalid rendered YAML", layer)
		}
		key := objectKey(obj)
		if key == "" {
			return fmt.Errorf("%s: rendered object lacks identity", layer)
		}
		if err := checkRenderedOwner(key, obj); err != nil {
			return err
		}
		if !effectiveKeys[key] {
			continue
		}
		if _, duplicate := actual[key]; duplicate {
			return fmt.Errorf("%s: duplicate audited object", layer)
		}
		data, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		actual[key] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("%s: audited runtime object set changed", layer)
	}
	for key, want := range expected {
		if actual[key] != want {
			return fmt.Errorf("%s: effective runtime premise changed: %s", layer, key)
		}
	}
	return nil
}

func checkRenderedOwner(key string, obj map[string]any) error {
	if err := checkWorldRenderedOwner(key, obj); err != nil {
		return err
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	// A new factory consumer, annotation, token consumer, or grant cannot borrow
	// another object's reviewed exception, including through provider patches.
	relevant := strings.Contains(string(data), "platform-coroot") || strings.Contains(string(data), "platform-kyverno") || strings.Contains(string(data), "[runtime-premise]")
	kind, _ := obj["kind"].(string)
	if kind == "ImageValidatingPolicy" || kind == "ImageVerificationConfig" {
		relevant = false
	}
	if relevant && !effectiveKeys[key] {
		return fmt.Errorf("%s: unreviewed effective runtime consumer", key)
	}
	if !effectiveKeys[key] {
		if err := checkIdentityOwner("rendered:"+key, obj); err != nil {
			return err
		}
	}
	return nil
}
