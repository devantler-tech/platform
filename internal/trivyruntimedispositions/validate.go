// Package trivyruntimedispositions binds runtime dispositions to audited source contracts.
package trivyruntimedispositions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devantler-tech/platform/internal/worldtrialprofile"
	"gopkg.in/yaml.v3"
)

// Complete parsed YAML objects and byte-exact factory/patch/script inputs bind
// image/source identity, Kubernetes ownership, mail ENV/ESO and token/RBAC.
// Stalwart aa47d8ef7bfba7200d3293198ac2592747b20e73 reads recovery-admin from
// ENV; the immutable image defines UID/GID2000 and an effective file capability.
// These are source/config receipts, not local Docker execution receipts.
var sourcePins = map[string]string{
	"k8s/bases/infrastructure/controllers/kyverno/role-binding.yaml":                                                   "8291bce476ab9a173840a4448d8bb7c86f9fa967f671e5d8d409c69f558074dc",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/cluster-role-binding.yaml":                      "7450a9a9b9cdd9bfa623bdb7075a77dfb6f56810b4101554b326c6c4c2b32216",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/cluster-role.yaml":                              "7c20ba75729481e8f4ee345e52c86cdbf6922867e19b64d2b585d20c0b3aad40",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/config-map.yaml":                                "2fd788c6dc1fb8c2cb0e078b14f00852ec5422a452ea22e014d337831a253aaa",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/cron-job.yaml":                                  "45198991b275a56c109bdab186e2d7402101b440b23ce98504e490f36928e44c",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role-binding.yaml":                              "e7195bbc7b202f64d4486964f8c0b9a1b4b2270e45dd491bbdb2f1626a53c891",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role.yaml":                                      "785d69eaa42bb6a8886b80e23730c71f7027f40cd07496d0beae7eff388ecd6f",
	"k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/service-account.yaml":                           "f0a74c5b30121990ece28557fe3f0f7ccf5ba65530dbde08a317eef4456b3b54",
	"k8s/providers/hetzner/infrastructure/cluster-policies/add-existing-csi-sidecar-baseline-context.yaml":             "f61d490f95bc4677557dfc854f786a01f6617df30c15210807df9229514b29e9",
	"k8s/providers/hetzner/infrastructure/cluster-policies/add-existing-longhorn-csi-plugin-baseline-context.yaml":     "c579b175eb0c259c26b68c77521c79c82d217cb9b58c182269026b2254c35a35",
	"k8s/providers/hetzner/infrastructure/cluster-policies/add-existing-longhorn-engine-image-baseline-context.yaml":   "cdabe031dab1917504a946756e14af61377eb025509464d0c873692b5fd5252e",
	"k8s/providers/hetzner/infrastructure/cluster-policies/remove-existing-longhorn-registrar-prestop.yaml":            "20a75e937dd4d08f2504ba92851796c8f474f3772dd9ad8c9d6cd9409e731694",
	"k8s/providers/hetzner/infrastructure/cluster-policies/remove-longhorn-registrar-prestop.yaml":                     "ec6132e525a3ae177fa969642f37f7c55ef65b2a43dc97822fef34451d421129",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml":                                      "45b1184364fd5c49789aeb3dd7caca6973bf49c4886a5adec0354883b0380674",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/role-binding-kyverno-mutate-csi-sidecars.yaml":          "431a227f7b1c8e9298ee87b3c2d152e15944a17dec25189bdc62871509fbb1de",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/role-binding-kyverno-mutate-longhorn-csi-plugin.yaml":   "39d56217018780a69d52d757eb9d7f9a1ca1030863d5441b00772f138d6e87b0",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/role-binding-kyverno-mutate-longhorn-engine-image.yaml": "f812392b5ed288eb71b054c347e9bc29cf73583c42d903c15c9f963f275e8de4",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-csi-sidecars.yaml":                  "eacf2fecdcb86e7aeaec65f3e4aa343519e313538e86ce5f00687a0f3e5804a2",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-csi-plugin.yaml":           "24b8879d119c3a773eeab6e92fc121f884928fe39db24b63e9d32ecdde150125",
	"k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-engine-image.yaml":         "2be24eea591c991dbc39e6bd5f2700b9985f43c6f8761880480b36cdd94c9474",
	"scripts/tests/test-csi-sidecar-baseline-context.sh":                                                               "29208b5b488fa41fafa4e9ddb6c83911ea6bc1d39dcb27ed273ba886f099bf28",
	"scripts/tests/test-longhorn-csi-plugin-baseline-context.sh":                                                       "b5182499e61a1650549e74b44593191f1d505e7335a86f95c55742348c2789ec",
	"scripts/tests/test-longhorn-engine-image-baseline-context.sh":                                                     "1ca3fcf253a4fcaa24bba070383b372f852eed92376b195122569452d7f43f51",
	"scripts/tests/test-longhorn-registrar-prestop-policy.sh":                                                          "a9c3c7d717416c305003cdcaf4403984178f9a3d8a7ceae7e119884b313d7180",
	"scripts/tests/test-prune-protected-orphan-alert.sh":                                                               "38ccdd1ad08491d4f8bbaa7d7ec302d7678ec78e393921709b5c49bfcc231119",
	".github/workflows/publish-coroot-checkpoint-hotfix.yaml":                                                          "66c283b272f2d63eb89491c8782dfa84f2f590fc8db4188efcf829f42b4da169",
	".github/workflows/publish-coroot-node-agent-hotfix.yaml":                                                          "ac80584af7be5496bfcf6dea9a6e8cce76be6aad958ec4440cd41707a378cf7a",
	".github/workflows/publish-kyverno-background-hotfix.yaml":                                                         "73112d5c0275efa936973216f7713e10926056f7b116c066baf34404934d0a74",
	".github/workflows/publish-kyverno-migration-runtime.yaml":                                                         "83d99649811f685837bc4eb726db3c8860d36a5a684c79c75f5a503030a7f8ba",
	".github/workflows/publish-kyverno-reports-hotfix.yaml":                                                            "ce8d6615e8fdbdb95b764de6290cff7e77dda12fbdf5cfdc7669f8bea3132bfe",
	".github/workflows/validate-coroot-security-progress.yaml":                                                         "2e184fdcec520ea7cacffc7477cd1e1ba43745e7cbc40cfc4b9258a552ef2f69",
	"k8s/bases/apps/private-mail/external-secret-admin.yaml":                                                           "241d98bdd9c933c60a005d456989bde29047aa2d191a93756211ac1f48278d8e",
	"k8s/bases/apps/private-mail/external-secret-generated-admin.yaml":                                                 "549d62c92df3307eb54d535b3b103ad9f19233c8d286d3cb723524213641410f",
	"k8s/bases/apps/private-mail/password-admin.yaml":                                                                  "3c6b0f0f3463b9852b6fa85b799b11572c38ee338e64506544543aac907de0ea",
	"k8s/bases/apps/private-mail/push-secret-admin.yaml":                                                               "2a20c7ea3bba077c6e8b611566d24a0c3b1171894c13755f160267629f06c735",
	"k8s/bases/apps/private-mail/service-account.yaml":                                                                 "02c56efad279dbfed0954493d96b7701040bab224f411e2ac98f9b900ecd12bc",
	"k8s/bases/apps/private-mail/stateful-set.yaml":                                                                    "b8bdb79277ae71fb81ba90bc3b7f74641c3cf886f17f951b0514993f87518aec",
	"k8s/bases/infrastructure/controllers/coroot/helm-release.yaml":                                                    "69f9e7aad699caaf86ac4a00e96776e30397fb9806949decc9ab6f16868a0e94",
	"k8s/bases/infrastructure/controllers/kyverno/BackgroundDownstreamCleanup.Dockerfile":                              "af2e19bc84eaedf0fcd9a8f58f2f69bfd3fcfb83074847fdcd420734bf31f8c7",
	"k8s/bases/infrastructure/controllers/kyverno/MigrationRuntime.Dockerfile":                                         "161d8b7e041b423be53feb36fa4287df7ee15f081c2cbc8704c84e13bc1f47c5",
	"k8s/bases/infrastructure/controllers/kyverno/ReportsWatchPlan.Dockerfile":                                         "d904e38111e89470a93e1fb9ad2f75471ccb88965ab71f191179b84ba57fb863",
	"k8s/bases/infrastructure/controllers/kyverno/background-v1.19.2-downstream-cleanup.patch":                         "ebf03e9b4925aa6cffc6fcc027201ad637dd1e7524ce6154831291ba9b58c1af",
	"k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml":                                                   "2ab1605e0e1f153794eba244fa1cdfecae16233afb544141c5e022543fa784ac",
	"k8s/bases/infrastructure/controllers/kyverno/migration-v1.19.2-runtime.patch":                                     "059a952f604d76807259593a1725dbaced749e320caa70be9a1209cda924f69d",
	"k8s/bases/infrastructure/controllers/kyverno/reports-v1.19.2-watch-plan.patch":                                    "1083364ba98e5e23976a5c3a3af47cf09ce6af7800a38c2397145a3d23600381",
	"k8s/bases/infrastructure/coroot/CorootCheckpoint.Dockerfile":                                                      "1a539e6779372d53752fc3a0a8c13aabb125f3ca47c6ca0f5b2c09a284bc7e98",
	"k8s/bases/infrastructure/coroot/CorootClusterAgentCheckpoint.Dockerfile":                                          "20e102017a0beb476c71c666c72e1df54e67d6823719e7dbf7868b5c0568f8e5",
	"k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile":                                                       "b089db04604a980071c99cadfac7a056975b658373aec06cc7c689753e038ebd",
	"k8s/bases/infrastructure/coroot/cluster-agent-v1.11.7-checkpoint-record.patch":                                    "0d31616b499782dd2cbc05fc64cb504277088f68b4c4256cd721eefacfa7b121",
	"k8s/bases/infrastructure/coroot/cluster-agent-v1.11.8-otel-log-api.patch":                                         "ab517878a6d2cd1a7006ade6e824f5cee13075f626d9f646052603cfb294f020",
	"k8s/bases/infrastructure/coroot/components/security-progress/cluster-role-binding.yaml":                           "2387f5266c89981d89f952f1f6b46d93188500dd99aeedd8b4170bbfa072ae2b",
	"k8s/bases/infrastructure/coroot/components/security-progress/cluster-role.yaml":                                   "fc60e73f00df723fbbfcf388d02e8780f8c5196e6fa695c5494aa20d69fb3d1b",
	"k8s/bases/infrastructure/coroot/components/security-progress/collector.sh":                                        "b7a2d35b3537e87dfcfeb5dc8a19dcc5a4061afffac9f11ae828457eebf7db8f",
	"k8s/bases/infrastructure/coroot/components/security-progress/config-map.yaml":                                     "8a497be7097f2508814f67b42cc94440d823f52632ff08d9e0d63decc12995b5",
	"k8s/bases/infrastructure/coroot/components/security-progress/deployment.yaml":                                     "a75568ab0c4539b4eb713ad5a4788e6b89fd732a0e9b718b9ebf5ae291a1d4d3",
	"k8s/bases/infrastructure/coroot/components/security-progress/service-account.yaml":                                "703cd6e16da10a69e2d97b5935bc7b3d54dd59e241e80697075fd447e4f32957",
	"k8s/bases/infrastructure/coroot/coroot-v1.28.1-checkpoint-record.patch":                                           "22e835745063d2b837f79591d04b951162e091d404f57c832bf852e35ed6d059",
	"k8s/bases/infrastructure/coroot/coroot.yaml":                                                                      "9c19d0dda6e08091db9decc373bb98a7d2d8f132a5106e547bfc27edcb9d4b77",
	"k8s/bases/infrastructure/coroot/node-agent-v1.38.1-alert-remediation.patch":                                       "4a56ce736ab8c4811d316eb7a7976a31c62c5f3728ce4d2063dcf9e0c86de961",
	"k8s/bases/infrastructure/vault-backup/cron-job.yaml":                                                              "658c6cdf315d54ead9c030e5909a1447ef13ec83445bfccd82c238c489ab6f40",
	"k8s/bases/infrastructure/vault-backup/job.yaml":                                                                   "2fcc4aa9b9c9501ddd01407672f946d43f580616aaf832fc1620428a48c4426f",
	"k8s/bases/infrastructure/vault-backup/service-account.yaml":                                                       "9b1513bb176dc77d4f1bc838e5efc7a7ec91173aa30c954db9f7eba7fe96e4f7",
	"k8s/bases/infrastructure/vault-config/job.yaml":                                                                   "89960be5d0ac22a65248397fbd9eee5ebf0eed4c7a0c4c5fe5b366d5d857e374",
	"scripts/check-kubescape-result-coverage.sh":                                                                       "87d2dd07675159f2c5965353fad80edacce1db3eca275b43f9d620d2b4a54faa",
}
var scopePins = map[string][]string{
	"KSV-0053": {"k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml", "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml", "k8s/providers/hetzner/apps/world-at-ruin/role-operator.yaml"},
	"KSV-0049": {"k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml", "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml", "k8s/bases/infrastructure/cluster-roles/tenant-base-edit.yaml", "k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role.yaml"},
	"KSV-0048": {"k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml", "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml", "k8s/bases/infrastructure/cluster-roles/tenant-base-edit.yaml", "k8s/bases/infrastructure/controllers/kyverno/role.yaml", "k8s/bases/infrastructure/resource-graph-definitions/webapp/cluster-role.yaml", "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-csi-sidecars.yaml", "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-csi-plugin.yaml", "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-longhorn-engine-image.yaml"},
	"DS-0026":  {"k8s/bases/infrastructure/controllers/kyverno/BackgroundDownstreamCleanup.Dockerfile", "k8s/bases/infrastructure/controllers/kyverno/MigrationRuntime.Dockerfile", "k8s/bases/infrastructure/controllers/kyverno/ReportsWatchPlan.Dockerfile", "k8s/bases/infrastructure/coroot/CorootCheckpoint.Dockerfile", "k8s/bases/infrastructure/coroot/CorootClusterAgentCheckpoint.Dockerfile", "k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile"},
	"DS-0002":  {"k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile"},
	"KSV-0020": {"k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml", "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml", "k8s/bases/infrastructure/vault-config/job.yaml", "k8s/bases/infrastructure/vault-backup/job.yaml", "k8s/bases/infrastructure/vault-backup/cron-job.yaml", "k8s/bases/apps/private-mail/stateful-set.yaml"},
	"KSV-0021": {"k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml", "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml", "k8s/bases/infrastructure/vault-config/job.yaml", "k8s/bases/infrastructure/vault-backup/job.yaml", "k8s/bases/infrastructure/vault-backup/cron-job.yaml", "k8s/bases/apps/private-mail/stateful-set.yaml"},
	"KSV-0022": {"k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml", "k8s/bases/apps/private-mail/stateful-set.yaml"},
	"KSV-0036": {"k8s/bases/infrastructure/coroot/components/security-progress/deployment.yaml", "k8s/bases/infrastructure/vault-backup/cron-job.yaml", "k8s/bases/infrastructure/vault-backup/job.yaml"},
}
var runtimePaths = map[string]bool{
	"k8s/bases/infrastructure/controllers/kyverno/BackgroundDownstreamCleanup.Dockerfile": true,
	"k8s/bases/infrastructure/controllers/kyverno/MigrationRuntime.Dockerfile":            true,
	"k8s/bases/infrastructure/controllers/kyverno/ReportsWatchPlan.Dockerfile":            true,
	"k8s/bases/infrastructure/coroot/CorootCheckpoint.Dockerfile":                         true,
	"k8s/bases/infrastructure/coroot/CorootClusterAgentCheckpoint.Dockerfile":             true,
	"k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile":                          true,
	"k8s/bases/apps/private-mail/stateful-set.yaml":                                       true,
	"k8s/bases/infrastructure/coroot/components/security-progress/deployment.yaml":        true,
	"k8s/bases/infrastructure/vault-backup/cron-job.yaml":                                 true,
	"k8s/bases/infrastructure/vault-backup/job.yaml":                                      true,
}

// Validate must succeed before and after the immutable native source scan.
func Validate(root string) error {
	if err := validateSource(root); err != nil {
		return err
	}
	if err := worldtrialprofile.Validate(root); err != nil {
		return err
	}
	return checkEffective(root)
}

func validateSource(root string) error {
	for path, want := range sourcePins {
		data, err := read(root, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".yaml") {
			obj, err := object(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			data, err = json.Marshal(obj)
			if err != nil {
				return err
			}
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != want {
			return fmt.Errorf("%s: runtime premise changed; review the disposition", path)
		}
	}
	data, err := read(root, ".trivyignore.yaml")
	if err != nil {
		return err
	}
	ignore, err := object(data)
	if err != nil {
		return err
	}
	if err = checkScopes(ignore); err != nil {
		return err
	}
	return checkOwnership(root)
}

func checkScopes(ignore map[string]any) error {
	entries, ok := ignore["misconfigurations"].([]any)
	if !ok {
		return fmt.Errorf("missing dispositions")
	}
	actual := map[string][]string{}
	for _, entry := range entries {
		row, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid disposition")
		}
		id, _ := row["id"].(string)
		if _, ok = scopePins[id]; !ok {
			continue
		}
		paths, ok := row["paths"].([]any)
		if !ok || len(paths) == 0 {
			return fmt.Errorf("%s: unscoped disposition", id)
		}
		for _, v := range paths {
			path, ok := v.(string)
			if !ok || path == "" {
				return fmt.Errorf("%s: invalid scope", id)
			}
			actual[id] = append(actual[id], path)
			newRBAC := (id == "KSV-0048" && strings.HasPrefix(path, "k8s/providers/hetzner/infrastructure/controllers/longhorn/role-kyverno-mutate-")) || (id == "KSV-0049" && path == "k8s/bases/infrastructure/controllers/prune-protected-orphan-alert/role.yaml") || (id == "KSV-0053" && path == "k8s/providers/hetzner/apps/world-at-ruin/role-operator.yaml")
			if id == "DS-0026" || id == "DS-0002" || id == "KSV-0036" || newRBAC || path == "k8s/bases/apps/private-mail/stateful-set.yaml" {
				statement, _ := row["statement"].(string)
				if !strings.Contains(statement, "[runtime-premise]") {
					return fmt.Errorf("%s: runtime statement missing", path)
				}
			}
		}
	}
	for id, paths := range scopePins {
		want, got := slices.Clone(paths), actual[id]
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			return fmt.Errorf("%s: scope changed, duplicated or missing", id)
		}
	}
	return nil
}

func checkOwnership(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("unreadable runtime ownership inventory")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", ".codex", ".agents", "node_modules", "megalinter-reports", "report", ".tokensave":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("linked runtime inventory input")
		}
		yamlFile := strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, ".yml")
		if !yamlFile && !strings.Contains(filepath.Base(rel), "Dockerfile") {
			return nil
		}
		data, err := read(root, rel)
		if err != nil {
			return err
		}
		_, pinned := sourcePins[rel]
		marker := bytes.Contains(data, []byte("[runtime-premise]"))
		protectedSkip := bytes.Contains(data, []byte("CKV_K8S_25=")) || bytes.Contains(data, []byte("CKV_K8S_35=")) || bytes.Contains(data, []byte("CKV_K8S_40=")) || bytes.Contains(data, []byte("checkov:skip=CKV_DOCKER_3:"))
		legacySkipOwner := rel == "k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml" || rel == "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml" || rel == "k8s/providers/docker/infrastructure/controllers/coredns/deployment.yaml"
		if protectedSkip && !pinned && !legacySkipOwner {
			return fmt.Errorf("%s: new runtime-class Checkov disposition", rel)
		}
		factory := bytes.Contains(data, []byte("platform-coroot")) || bytes.Contains(data, []byte("platform-kyverno"))
		verificationOnly := false
		if factory && (rel == "talos/cluster/verify-first-party-images.yaml" || rel == "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml") {
			obj, parseErr := object(data)
			verificationOnly = parseErr == nil && (obj["kind"] == "ImageVerificationConfig" || obj["kind"] == "ImageValidatingPolicy")
		}
		if (marker || (factory && !verificationOnly)) && !pinned && rel != ".trivyignore.yaml" {
			return fmt.Errorf("%s: new or moved runtime ownership/annotation", rel)
		}
		if !yamlFile || (!bytes.Contains(data, []byte("vault-snapshot")) && !bytes.Contains(data, []byte("coroot-security-progress")) && !bytes.Contains(data, []byte("RoleBinding")) && !bytes.Contains(data, []byte("prune-protected-orphan-alert")) && !bytes.Contains(data, []byte("kyverno-background-controller")) && !bytes.Contains(data, []byte("kyverno:background-controller:mutate")) && !bytes.Contains(data, []byte("targets:"))) {
			return nil
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var node yaml.Node
			if e := decoder.Decode(&node); e == io.EOF {
				break
			} else if e != nil {
				return fmt.Errorf("%s: invalid ownership YAML", rel)
			}
			if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
				continue // JSON-patch sequences are checked through effective rendering.
			}
			var obj map[string]any
			if e := node.Decode(&obj); e != nil {
				return fmt.Errorf("%s: invalid ownership mapping", rel)
			}
			if err = checkIdentityOwner(rel, obj); err != nil {
				return err
			}
		}
		return nil
	})
}

func checkIdentityOwner(path string, obj map[string]any) error {
	if err := checkRBACOwner(path, obj); err != nil {
		return err
	}
	protected := func(v any) bool { return v == "vault-snapshot" || v == "coroot-security-progress" }
	kind, _ := obj["kind"].(string)
	if kind == "List" {
		items, _ := obj["items"].([]any)
		for _, item := range items {
			child, _ := item.(map[string]any)
			if err := checkIdentityOwner(path, child); err != nil {
				return err
			}
		}
	}
	if kind == "RoleBinding" || kind == "ClusterRoleBinding" {
		ref, _ := obj["roleRef"].(map[string]any)
		if protected(ref["name"]) && path != "k8s/bases/infrastructure/coroot/components/security-progress/cluster-role-binding.yaml" {
			return fmt.Errorf("%s: new projected-identity role consumer", path)
		}
		subjects, _ := obj["subjects"].([]any)
		for _, s := range subjects {
			m, _ := s.(map[string]any)
			name, _ := m["name"].(string)
			if m["kind"] == "Group" && (name == "system:authenticated" || name == "system:serviceaccounts" || name == "system:serviceaccounts:observability" || name == "system:serviceaccounts:openbao") || m["kind"] == "User" && (name == "system:serviceaccount:observability:coroot-security-progress" || name == "system:serviceaccount:openbao:vault-snapshot") {
				return fmt.Errorf("%s: new broad projected-identity RBAC binding", path)
			}
			if m["kind"] == "ServiceAccount" && protected(m["name"]) && path != "k8s/bases/infrastructure/coroot/components/security-progress/cluster-role-binding.yaml" {
				return fmt.Errorf("%s: new projected-identity RBAC binding", path)
			}
		}
	}
	if kind == "Pod" || kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" || kind == "Job" || kind == "CronJob" {
		spec, _ := obj["spec"].(map[string]any)
		if kind == "CronJob" {
			job, _ := spec["jobTemplate"].(map[string]any)
			spec, _ = job["spec"].(map[string]any)
		}
		if kind != "Pod" {
			template, _ := spec["template"].(map[string]any)
			spec, _ = template["spec"].(map[string]any)
		}
		if protected(spec["serviceAccountName"]) && !runtimePaths[path] {
			return fmt.Errorf("%s: new projected-identity consumer", path)
		}
	}
	return nil
}

func object(data []byte) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("invalid YAML")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("expected one YAML document")
	}
	var obj map[string]any
	if err := node.Decode(&obj); err != nil || len(obj) == 0 {
		return nil, fmt.Errorf("invalid or duplicate YAML mapping")
	}
	return obj, nil
}

func read(root, rel string) ([]byte, error) {
	current := filepath.Clean(root)
	info, err := os.Lstat(current)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("runtime root must be a real directory")
	}
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid runtime path")
		}
		current = filepath.Join(current, part)
		info, err = os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: missing or linked runtime source", rel)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("%s: invalid runtime parent", rel)
		}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 || info.Size() > 4<<20 {
		return nil, fmt.Errorf("%s: unreadable or nonregular runtime source", rel)
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s: runtime source changed while opening", rel)
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, fmt.Errorf("%s: runtime source read failed", rel)
	}
	after, err := os.Lstat(current)
	if err != nil || !os.SameFile(info, after) || after.ModTime() != info.ModTime() || after.Size() != info.Size() {
		return nil, fmt.Errorf("%s: runtime source changed during read", rel)
	}
	return data, nil
}
