"""Regression tests for the production Flux GHCR credential bridge."""

from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[2]
AGENT_INSTRUCTIONS = ROOT / "AGENTS.md"
HELPER = ROOT / "scripts" / "refresh-flux-ghcr-auth.sh"
GHCR_AUTH_LIB = ROOT / "scripts" / "ghcr-auth-lib.sh"
KSAIL_PULL_WRAPPER = ROOT / "scripts" / "run-ksail-prod-with-pull-auth.sh"
KSAIL_PROD_CONFIG = ROOT / "ksail.prod.yaml"
ACTION = ROOT / ".github" / "actions" / "deploy-prod" / "action.yml"
DR_REBUILD = ROOT / ".github" / "workflows" / "dr-rebuild.yaml"
CI_WORKFLOW = ROOT / ".github" / "workflows" / "ci.yaml"
DR_RUNBOOK = ROOT / "docs" / "dr" / "runbook.md"
KSAIL_OPERATOR_HELM_RELEASE = (
    ROOT
    / "k8s"
    / "bases"
    / "infrastructure"
    / "controllers"
    / "ksail-operator"
    / "helm-release.yaml"
)
KSAIL_OPERATOR_VERSION = next(
    line.split(":", 1)[1].strip()
    for line in KSAIL_OPERATOR_HELM_RELEASE.read_text(encoding="utf-8").splitlines()
    if line.strip().startswith("version:")
)
# spec.cluster.talos.version is the source of truth the workflows' TALOS_VERSION
# must track; deriving it here keeps the drift guard honest across version bumps.
KSAIL_PROD_TALOS_VERSION = next(
    line.split(":", 1)[1].strip().lstrip("v")
    for line in KSAIL_PROD_CONFIG.read_text(encoding="utf-8").splitlines()
    if line.strip().startswith("version:")
)
PROVIDER_UPJET_UNIFI = (
    ROOT
    / "k8s"
    / "providers"
    / "hetzner"
    / "infrastructure"
    / "crossplane"
    / "provider-upjet-unifi.yaml"
)
TALOS_GHCR_AUTH = ROOT / "talos" / "cluster" / "authenticate-ghcr-pulls.yaml"
TALOS_GHCR_REVISION = (
    ROOT / "talos" / "cluster" / "mark-ghcr-pull-revision.yaml"
)


class RefreshFluxGhcrAuthTests(unittest.TestCase):
    """Exercise the helper with fake external commands and no real secrets."""

    def setUp(self) -> None:
        """Create isolated command fakes and capture files for each test."""
        self.temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp_dir.cleanup)
        self.workspace = Path(self.temp_dir.name)
        self.bin_dir = self.workspace / "bin"
        self.bin_dir.mkdir()
        self.decrypted_config = self.workspace / "decrypted-config.json"
        self.encrypted_secret = self.workspace / "secret.enc.yaml"
        self.patch_capture = self.workspace / "patch.json"
        self.variables_patch_capture = self.workspace / "variables-patch.json"
        self.kubectl_called = self.workspace / "kubectl-called"
        self.output_path_log = self.workspace / "ksail-output-path"
        self.registry_read_log = self.workspace / "registry-reads"
        self.fanout_log = self.workspace / "fanout-log"
        self.talos_log = self.workspace / "talos-log"
        self.talos_patch_path_log = self.workspace / "talos-patch-path"
        self.operation_log = self.workspace / "operation-log"
        self.ksail_token_capture = self.workspace / "ksail-token"
        self.ksail_username_capture = self.workspace / "ksail-username"
        self.ksail_revision_capture = self.workspace / "ksail-revision"
        self.ksail_command_capture = self.workspace / "ksail-command"
        self.ksail_config_path_capture = self.workspace / "ksail-config-path"
        self.ksail_registry_capture = self.workspace / "ksail-registry"
        self.ksail_registry_override_capture = (
            self.workspace / "ksail-registry-override"
        )
        self.sync_state_dir = self.workspace / "sync-state"
        self.sync_state_dir.mkdir()
        self._write_encrypted_secret("ENC[AES256_GCM,data:fixture-one]")

        self._write_executable(
            "ksail",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            arguments=" $* "
            selector='["stringData"]["ghcr_dockerconfigjson"]'
            if [[ "$arguments" == *" workload cipher decrypt "* ]]; then
              [[ "$arguments" == *" --extract $selector "* ]]
              output=""
              while (($#)); do
                case "$1" in
                  --output)
                    output="$2"
                    shift 2
                    ;;
                  *) shift ;;
                esac
              done
              test -n "$output"
              printf '%s' "$output" > "$KSAIL_OUTPUT_PATH_LOG"
              cp "$FAKE_DECRYPTED_CONFIG" "$output"
              exit 0
            fi

            if [[ "$arguments" == *" cluster create "* \
              || "$arguments" == *" cluster update "* \
              || "$arguments" == *" workload push "* \
              || "$arguments" == *" workload reconcile "* ]]; then
              config=""
              previous=""
              for argument in "$@"; do
                if [[ "$previous" == --config ]]; then
                  config="$argument"
                  break
                fi
                previous="$argument"
              done
              test -f "$config"
              registry="$(yq -er '.spec.cluster.localRegistry.registry' "$config")"
              test "$registry" = \
                'devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests'
              registry_override="${KSAIL_SPEC_CLUSTER_LOCALREGISTRY_REGISTRY:-}"
              if [[ "$arguments" == *" workload push "* ]]; then
                test -z "$registry_override"
              else
                test "$registry_override" = \
                  '${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests'
              fi
              test -n "${GHCR_TOKEN:-}"
              test -n "${GHCR_USERNAME:-}"
              test "${GHCR_PULL_REVISION:-}" != ""
              printf '%s' "$GHCR_TOKEN" > "$KSAIL_TOKEN_CAPTURE"
              printf '%s' "$GHCR_USERNAME" > "$KSAIL_USERNAME_CAPTURE"
              printf '%s' "$GHCR_PULL_REVISION" > "$KSAIL_REVISION_CAPTURE"
              printf '%s\n' "$*" > "$KSAIL_COMMAND_CAPTURE"
              printf '%s' "$config" > "$KSAIL_CONFIG_PATH_CAPTURE"
              printf '%s' "$registry" > "$KSAIL_REGISTRY_CAPTURE"
              printf '%s' "$registry_override" > "$KSAIL_REGISTRY_OVERRIDE_CAPTURE"
              exit 0
            fi

            echo "unexpected ksail invocation" >&2
            exit 92
            """,
        )
        self._write_executable(
            "talosctl",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            arguments=" $* "
            node=""
            patch_file=""
            previous=""
            for argument in "$@"; do
              if [[ "$previous" == --nodes ]]; then
                node="$argument"
              fi
              case "$argument" in
                --nodes=*) node="${argument#*=}" ;;
                --patch-file=*) patch_file="${argument#*=}" ;;
              esac
              previous="$argument"
            done
            test -n "$node"

            if [[ "$arguments" == *" etcd status "* ]]; then
              if [[ "$node" == "${FAKE_ETCD_STATUS_FAIL_NODE:-disabled}" ]]; then
                echo "etcd status failed" >&2
                exit 51
              fi
              printf 'NODE MEMBER DB-SIZE\n%s member-id 1MB\n' "$node"
              exit 0
            fi

            if [[ "$arguments" == *" etcd alarm list "* ]]; then
              if [[ "$node" == "${FAKE_ETCD_ALARM_READ_FAIL_NODE:-disabled}" ]]; then
                echo "etcd alarm read failed" >&2
                exit 52
              fi
              printf 'NODE MEMBER ALARM\n'
              if [[ "$node" == "${FAKE_ETCD_ALARM_NODE:-disabled}" ]]; then
                printf '%s member-id NOSPACE\n' "$node"
              fi
              exit 0
            fi

            if [[ "$arguments" == *" patch machineconfig "* ]]; then
              [[ "$arguments" == *" --mode=no-reboot "* ]]
              test -f "$patch_file"
              printf '%s' "$patch_file" > "$TALOS_PATCH_PATH_LOG"
              if jq -e '.kind == "RegistryAuthConfig"' "$patch_file" >/dev/null; then
                jq -e \
                  --arg username "$EXPECTED_PULL_USERNAME" \
                  --arg token "$EXPECTED_PULL_TOKEN" '
                  .apiVersion == "v1alpha1"
                  and .kind == "RegistryAuthConfig"
                  and .name == "ghcr.io"
                  and .username == $username
                  and .password == $token
                ' "$patch_file" >/dev/null
                printf 'talos-auth:%s\n' "$node" >> "$TALOS_LOG"
                printf 'talos-auth:%s\n' "$node" >> "$OPERATION_LOG"
                if [[ "$node" == "${FAKE_TALOS_FAIL_NODE:-disabled}" \
                  && "${FAKE_TALOS_FAIL_OPERATION:-auth}" == auth ]]; then
                  echo "talos auth failed with $EXPECTED_PULL_TOKEN" >&2
                  exit 45
                fi
                touch "$FAKE_SYNC_STATE_DIR/talos-auth-${node}"
                exit 0
              fi

              test -f "$FAKE_SYNC_STATE_DIR/talos-auth-${node}"
              test -f "$FAKE_SYNC_STATE_DIR/talos-reboot-${node}"
              test -f "$FAKE_SYNC_STATE_DIR/talos-remove-${node}"
              test -f "$FAKE_SYNC_STATE_DIR/talos-pull-${node}"
              jq -e --arg revision "$EXPECTED_GHCR_REVISION" '
                .machine.nodeAnnotations[
                  "platform.devantler.tech/ghcr-pull-verified-revision-v2"
                ] == $revision
              ' "$patch_file" >/dev/null
              jq -e --arg image "$EXPECTED_KSAIL_TARGET_IMAGE" '
                .machine.nodeAnnotations[
                  "platform.devantler.tech/ghcr-pull-verified-image-v2"
                ] == $image
              ' "$patch_file" >/dev/null
              printf 'talos-revision:%s\n' "$node" >> "$TALOS_LOG"
              printf 'talos-revision:%s\n' "$node" >> "$OPERATION_LOG"
              if [[ "$node" == "${FAKE_TALOS_FAIL_NODE:-disabled}" \
                && "${FAKE_TALOS_FAIL_OPERATION:-auth}" == revision ]]; then
                echo "talos revision failed" >&2
                exit 48
              fi
              touch "$FAKE_SYNC_STATE_DIR/talos-revision-${node}"
              exit 0
            fi

            # A running containerd only adopts new registry auth by restarting,
            # and Talos refuses `service cri restart`, so the node must reboot.
            # The auth patch must already have landed, and the reboot must drain
            # (cordon + evict under PDB) rather than killing the pods with it.
            if [[ "$arguments" == *" reboot "* ]]; then
              test -f "$FAKE_SYNC_STATE_DIR/talos-auth-${node}"
              [[ "$arguments" == *" --drain "* ]]
              printf 'talos-reboot:%s\n' "$node" >> "$TALOS_LOG"
              printf 'talos-reboot:%s\n' "$node" >> "$OPERATION_LOG"
              if [[ "$node" == "${FAKE_TALOS_FAIL_NODE:-disabled}" \
                && "${FAKE_TALOS_FAIL_OPERATION:-auth}" == reboot ]]; then
                echo "talos reboot failed" >&2
                exit 49
              fi
              touch "$FAKE_SYNC_STATE_DIR/talos-reboot-${node}"
              exit 0
            fi

            if [[ "$arguments" == *" image remove "* ]]; then
              test -f "$FAKE_SYNC_STATE_DIR/talos-auth-${node}"
              # The cache is only worth clearing once containerd has reloaded the
              # credential — i.e. after the reboot.
              test -f "$FAKE_SYNC_STATE_DIR/talos-reboot-${node}"
              [[ "$arguments" == *" --namespace cri "* ]]
              image=""
              previous=""
              for argument in "$@"; do
                if [[ "$previous" == remove ]]; then
                  image="$argument"
                  break
                fi
                previous="$argument"
              done
              test -n "$image"
              printf 'talos-remove:%s:%s\n' "$node" "$image" >> "$TALOS_LOG"
              printf 'talos-remove:%s:%s\n' "$node" "$image" >> "$OPERATION_LOG"
              if [[ "$node" == "${FAKE_TALOS_IMAGE_ABSENT_NODE:-disabled}" ]]; then
                touch "$FAKE_SYNC_STATE_DIR/talos-remove-${node}"
                echo "rpc error: code = NotFound desc = image ${image} not found" >&2
                exit 1
              fi
              if [[ "$node" == "${FAKE_TALOS_FAIL_NODE:-disabled}" \
                && "${FAKE_TALOS_FAIL_OPERATION:-auth}" == remove ]]; then
                echo "talos remove failed" >&2
                exit 49
              fi
              touch "$FAKE_SYNC_STATE_DIR/talos-remove-${node}"
              exit 0
            fi

            if [[ "$arguments" == *" image pull "* ]]; then
              test -f "$FAKE_SYNC_STATE_DIR/talos-auth-${node}"
              # The pull check is only meaningful once containerd has actually
              # reloaded the credential (the reboot) and the cached copy is gone
              # (the remove), so the pull must complete a real registry round-trip.
              test -f "$FAKE_SYNC_STATE_DIR/talos-reboot-${node}"
              test -f "$FAKE_SYNC_STATE_DIR/talos-remove-${node}"
              [[ "$arguments" == *" --namespace cri "* ]]
              image=""
              previous=""
              for argument in "$@"; do
                if [[ "$previous" == pull ]]; then
                  image="$argument"
                  break
                fi
                previous="$argument"
              done
              test -n "$image"
              printf 'talos-pull:%s:%s\n' "$node" "$image" >> "$TALOS_LOG"
              printf 'talos-pull:%s:%s\n' "$node" "$image" >> "$OPERATION_LOG"
              if [[ "$node" == "${FAKE_TALOS_FAIL_NODE:-disabled}" \
                && "${FAKE_TALOS_FAIL_OPERATION:-auth}" == pull ]]; then
                echo "talos pull failed with $EXPECTED_PULL_TOKEN" >&2
                exit 47
              fi
              touch "$FAKE_SYNC_STATE_DIR/talos-pull-${node}"
              exit 0
            fi

            echo "unexpected talosctl invocation" >&2
            exit 93
            """,
        )
        self._write_executable(
            "curl",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            test "$1" = --disable
            shift
            config=""
            output=""
            scope=""
            url=""
            while (($#)); do
              case "$1" in
                --config)
                  config="$2"
                  shift 2
                  ;;
                --output)
                  output="$2"
                  shift 2
                  ;;
                --data-urlencode)
                  case "$2" in scope=*) scope="${2#scope=}" ;; esac
                  shift 2
                  ;;
                --write-out|--header)
                  shift 2
                  ;;
                --silent|--show-error|--get)
                  shift
                  ;;
                https://*)
                  url="$1"
                  shift
                  ;;
                *)
                  echo "unexpected curl argument: $1" >&2
                  exit 90
                  ;;
              esac
            done
            test -f "$config"
            test -n "$output"
            test -n "$url"

            if [[ "$url" == https://ghcr.io/token ]]; then
              test -n "$scope"
              grep -q '^user = ' "$config"
              printf '{"token":"fixture-registry-token"}' > "$output"
              printf '200'
              exit 0
            fi

            manifest_path="${url#https://ghcr.io/v2/}"
            repository="${manifest_path%/manifests/*}"
            reference="${manifest_path##*/manifests/}"
            test "$repository" != "$manifest_path"
            test "$reference" != "$manifest_path"
            grep -q 'Authorization: Bearer fixture-registry-token' "$config"
            printf '%s:%s\n' "$repository" "$reference" >> "$REGISTRY_READ_LOG"
            if [[ "$repository" == "${FAKE_CURL_DENY_REPOSITORY:-disabled}" ]]; then
              printf '403'
            else
              printf '200'
            fi
            """,
        )
        self._write_executable(
            "kubectl",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            arguments=" $* "
            [[ "$arguments" == *" --context admin@prod "* ]]
            namespace=""
            patch_file=""
            previous=""
            for argument in "$@"; do
              if [[ "$previous" == --namespace ]]; then
                namespace="$argument"
              fi
              case "$argument" in
                --namespace=*) namespace="${argument#*=}" ;;
                --patch-file=*) patch_file="${argument#*=}" ;;
              esac
              previous="$argument"
            done
            touch "$KUBECTL_CALLED"

            if [[ "$arguments" == *" get nodes "* ]]; then
              if [[ "${FAKE_NODE_DISCOVERY_FAIL:-false}" == true ]]; then
                echo 'node discovery failed' >&2
                exit 46
              fi
              if [[ -n "${FAKE_NODE_JSON:-}" ]]; then
                printf '%s\n' "$FAKE_NODE_JSON"
                exit 0
              fi
              jq -n \
                --arg revision "$EXPECTED_GHCR_REVISION" \
                --arg current "${FAKE_TALOS_NODES_CURRENT:-false}" \
                --arg verified_image \
                  "${FAKE_TALOS_VERIFIED_IMAGE:-$EXPECTED_KSAIL_TARGET_IMAGE}" \
                --arg target_image "$EXPECTED_KSAIL_TARGET_IMAGE" '
                {
                  items: [
                    {
                      metadata: {
                        name: "prod-worker-1",
                        labels: {},
                        annotations: {
                          "platform.devantler.tech/ghcr-pull-desired-revision":
                            $revision
                        }
                      },
                      status: {addresses: [
                        {type: "InternalIP", address: "10.0.0.2"},
                        {type: "ExternalIP", address: "198.51.100.2"}
                      ]}
                    },
                    {
                      metadata: {
                        name: "prod-control-plane-1",
                        labels: {
                          "node-role.kubernetes.io/control-plane": ""
                        },
                        annotations: {
                          "platform.devantler.tech/ghcr-pull-desired-revision":
                            $revision
                        }
                      },
                      status: {addresses: [
                        {type: "InternalIP", address: "10.0.0.1"},
                        {type: "ExternalIP", address: "198.51.100.1"}
                      ]}
                    },
                    {
                      metadata: {
                        name: "prod-control-plane-2",
                        labels: {
                          "node-role.kubernetes.io/control-plane": ""
                        },
                        annotations: {
                          "platform.devantler.tech/ghcr-pull-desired-revision":
                            $revision,
                          "platform.devantler.tech/ghcr-pull-verified-revision-v2":
                            $revision,
                          "platform.devantler.tech/ghcr-pull-verified-image-v2":
                            $target_image
                        }
                      },
                      status: {
                        addresses: [
                          {type: "InternalIP", address: "10.0.0.3"},
                          {type: "ExternalIP", address: "198.51.100.3"}
                        ],
                        conditions: [{type: "Ready", status: "True"}]
                      }
                    },
                    {
                      metadata: {
                        name: "prod-control-plane-3",
                        labels: {
                          "node-role.kubernetes.io/control-plane": ""
                        },
                        annotations: {
                          "platform.devantler.tech/ghcr-pull-desired-revision":
                            $revision,
                          "platform.devantler.tech/ghcr-pull-verified-revision-v2":
                            $revision,
                          "platform.devantler.tech/ghcr-pull-verified-image-v2":
                            $target_image
                        }
                      },
                      status: {
                        addresses: [
                          {type: "InternalIP", address: "10.0.0.4"},
                          {type: "ExternalIP", address: "198.51.100.4"}
                        ],
                        conditions: [{type: "Ready", status: "True"}]
                      }
                    }
                  ]
                }
                | if $current == "true" then
                    .items[0:2][].metadata.annotations[
                      "platform.devantler.tech/ghcr-pull-verified-revision-v2"
                    ] = $revision
                    | .items[0:2][].metadata.annotations[
                      "platform.devantler.tech/ghcr-pull-verified-image-v2"
                    ] = $verified_image
                  else . end
              '
              exit 0
            fi

            # Cordon bookkeeping around a GHCR-auth reboot. Cluster-scoped, so
            # both must be handled before the namespace guard below.
            # FAKE_CORDONED_NODES is a space-separated list of already-cordoned
            # node names, i.e. nodes an operator had deliberately made
            # unschedulable before this run.
            if [[ "$arguments" == *" get node "* ]]; then
              node_target=""
              previous=""
              for argument in "$@"; do
                if [[ "$previous" == node ]]; then
                  node_target="$argument"
                  break
                fi
                previous="$argument"
              done
              test -n "$node_target"
              if [[ " ${FAKE_CORDONED_NODES:-} " == *" ${node_target} "* ]]; then
                printf 'true'
              fi
              exit 0
            fi

            if [[ "$arguments" == *" cordon "* ]]; then
              node_target=""
              previous=""
              for argument in "$@"; do
                if [[ "$previous" == cordon ]]; then
                  node_target="$argument"
                  break
                fi
                previous="$argument"
              done
              test -n "$node_target"
              printf 'node-cordon:%s\n' "$node_target" >> "$OPERATION_LOG"
              exit 0
            fi

            # Node readiness gate after a GHCR-auth reboot. Cluster-scoped, so it
            # must be handled before the namespace guard below.
            if [[ "$arguments" == *" wait "* ]]; then
              [[ "$arguments" == *" --for=condition=Ready "* ]]
              node_target=""
              for argument in "$@"; do
                case "$argument" in
                  node/*) node_target="${argument#node/}" ;;
                esac
              done
              test -n "$node_target"
              printf 'node-ready:%s\n' "$node_target" >> "$OPERATION_LOG"
              if [[ "$node_target" == "${FAKE_NODE_READY_FAIL_NODE:-disabled}" ]]; then
                echo 'node did not become ready' >&2
                exit 50
              fi
              exit 0
            fi

            test -n "$namespace"

            if [[ "$arguments" == *" api-resources "* ]]; then
              [[ "$arguments" == *" --api-group=external-secrets.io "* ]]
              if [[ "${FAKE_FANOUT_CRDS_ABSENT:-false}" != true ]]; then
                printf '%s\n' \
                  'externalsecrets.external-secrets.io' \
                  'pushsecrets.external-secrets.io'
              fi
              exit 0
            fi

            if [[ "$arguments" == *" patch secret ksail-registry-credentials "* ]]; then
              [[ "$arguments" == *" --type=merge "* ]]
              test -n "$patch_file"
              cp "$patch_file" "$PATCH_CAPTURE"
              printf 'root-patch\n' >> "$OPERATION_LOG"
              if [[ "${FAKE_KUBECTL_FAIL:-false}" == true ]]; then
                echo 'cluster patch failed' >&2
                exit 43
              fi
              echo 'secret/ksail-registry-credentials patched'
              exit 0
            fi

            if [[ "$arguments" == *" get secret variables-base "* ]]; then
              [[ "$arguments" == *" --ignore-not-found "* ]]
              if [[ "${FAKE_VARIABLES_BASE_ABSENT:-false}" != true ]]; then
                echo 'secret/variables-base'
              fi
              exit 0
            fi

            if [[ "$arguments" == *" patch secret variables-base "* ]]; then
              [[ "$arguments" == *" --type=merge "* ]]
              test -n "$patch_file"
              cp "$patch_file" "$VARIABLES_PATCH_CAPTURE"
              printf 'variables-patch\n' >> "$OPERATION_LOG"
              echo 'secret/variables-base patched'
              exit 0
            fi

            kind=""
            name=""
            if [[ "$arguments" == *" pushsecret seed-ghcr "* ]]; then
              kind=pushsecret
              name=seed-ghcr
            elif [[ "$arguments" == *" externalsecret ghcr-auth "* ]]; then
              kind=externalsecret
              name=ghcr-auth
            fi

            if [[ -n "$kind" && "$arguments" == *" get $kind $name "* \
              && "$arguments" == *" --ignore-not-found "* ]]; then
              resource="$kind/$namespace/$name"
              if [[ "$resource" != "${FAKE_MISSING_FANOUT_RESOURCE:-disabled}" ]]; then
                echo "$kind/$name"
              fi
              exit 0
            fi

            if [[ -n "$kind" \
              && "$kind/$namespace/$name" == "${FAKE_MISSING_FANOUT_RESOURCE:-disabled}" ]]; then
              echo "$kind/$name not found" >&2
              exit 44
            fi

            if [[ -n "$kind" && "$arguments" == *" get $kind $name "* ]]; then
              marker="$FAKE_SYNC_STATE_DIR/${kind}-${namespace}-${name}"
              annotated_marker="${marker}-annotated"
              refresh_time=2026-07-13T00:00:00Z
              resource_version=1
              if [[ -f "$annotated_marker" ]]; then
                resource_version=2
              fi
              if [[ -f "$marker" && "${FAKE_SYNC_SAME_REFRESH_TIME:-false}" != true ]]; then
                refresh_time=2026-07-13T00:00:01Z
              fi
              if [[ -f "$marker" ]]; then
                resource_version=3
              fi
              printf '{"metadata":{"resourceVersion":"%s"},"status":{"refreshTime":"%s","conditions":[{"type":"Ready","status":"True"}]}}\n' "$resource_version" "$refresh_time"
              exit 0
            fi

            if [[ -n "$kind" && "$arguments" == *" annotate $kind $name "* ]]; then
              resource="$kind/$namespace/$name"
              printf '%s\n' "$resource" >> "$FANOUT_LOG"
              printf 'fanout:%s\n' "$resource" >> "$OPERATION_LOG"
              marker="$FAKE_SYNC_STATE_DIR/${kind}-${namespace}-${name}"
              touch "${marker}-annotated"
              if [[ "$resource" != "${FAKE_SYNC_STALL_RESOURCE:-disabled}" ]]; then
                touch "$marker"
              fi
              printf '{"metadata":{"resourceVersion":"2"}}\n'
              exit 0
            fi

            if [[ "$arguments" == *" get secret ghcr-auth "* ]]; then
              if [[ "$namespace" == "${FAKE_CONSUMER_MISMATCH_NAMESPACE:-disabled}" ]]; then
                encoded=$(printf '%s' '{"auths":{}}' | base64 | tr -d '\r\n')
              else
                encoded=$(jq -r '.data.ghcr_dockerconfigjson' "$VARIABLES_PATCH_CAPTURE")
              fi
              jq -n --arg encoded "$encoded" '{data:{".dockerconfigjson":$encoded}}'
              exit 0
            fi

            echo "unexpected kubectl invocation: $arguments" >&2
            exit 91
            """,
        )

    def _write_executable(self, name: str, body: str) -> None:
        """Install an executable command fake in the isolated test PATH."""
        path = self.bin_dir / name
        path.write_text(textwrap.dedent(body).lstrip(), encoding="utf-8")
        path.chmod(0o755)

    def _write_encrypted_secret(self, ciphertext: str) -> None:
        """Write a non-secret SOPS ciphertext fixture for revision tests."""
        self.encrypted_ciphertext = ciphertext
        self.encrypted_secret.write_text(
            json.dumps(
                {"stringData": {"ghcr_dockerconfigjson": ciphertext}}
            ),
            encoding="utf-8",
        )

    @staticmethod
    def _expected_credentials(config: object) -> tuple[str, str]:
        """Extract expected credentials from a valid Docker config fixture."""
        try:
            registry = config["auths"]["ghcr.io"]  # type: ignore[index]
            username = registry.get("username")
            password = registry.get("password")
            if username and password:
                return str(username), str(password)
            decoded = base64.b64decode(registry["auth"]).decode()
            decoded_username, decoded_password = decoded.split(":", 1)
            return decoded_username, decoded_password
        except (KeyError, TypeError, ValueError):
            return "unused", "unused"

    def _expected_revision(self) -> str:
        """Match the helper's SHA-256 of the yq-emitted ciphertext scalar."""
        return hashlib.sha256(
            f"{self.encrypted_ciphertext}\n".encode()
        ).hexdigest()

    def _run_helper(
        self,
        config: object,
        helper_args: tuple[str, ...] = (),
        **environment_overrides: str,
    ) -> subprocess.CompletedProcess[str]:
        """Run the credential bridge against a supplied Docker config fixture."""
        self.decrypted_config.write_text(json.dumps(config), encoding="utf-8")
        for marker in (
            self.patch_capture,
            self.variables_patch_capture,
            self.kubectl_called,
            self.output_path_log,
            self.registry_read_log,
            self.fanout_log,
            self.talos_log,
            self.talos_patch_path_log,
            self.operation_log,
            self.ksail_token_capture,
            self.ksail_username_capture,
            self.ksail_revision_capture,
            self.ksail_command_capture,
            self.ksail_config_path_capture,
            self.ksail_registry_capture,
            self.ksail_registry_override_capture,
        ):
            marker.unlink(missing_ok=True)
        for marker in self.sync_state_dir.iterdir():
            marker.unlink()
        expected_username, expected_token = self._expected_credentials(config)
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": f"{self.bin_dir}:{environment['PATH']}",
                "FAKE_DECRYPTED_CONFIG": str(self.decrypted_config),
                "FLUX_GHCR_SECRET_FILE": str(self.encrypted_secret),
                "PATCH_CAPTURE": str(self.patch_capture),
                "VARIABLES_PATCH_CAPTURE": str(self.variables_patch_capture),
                "KUBECTL_CALLED": str(self.kubectl_called),
                "KSAIL_OUTPUT_PATH_LOG": str(self.output_path_log),
                "REGISTRY_READ_LOG": str(self.registry_read_log),
                "FANOUT_LOG": str(self.fanout_log),
                "TALOS_LOG": str(self.talos_log),
                "TALOS_PATCH_PATH_LOG": str(self.talos_patch_path_log),
                "OPERATION_LOG": str(self.operation_log),
                "KSAIL_TOKEN_CAPTURE": str(self.ksail_token_capture),
                "KSAIL_USERNAME_CAPTURE": str(self.ksail_username_capture),
                "KSAIL_REVISION_CAPTURE": str(self.ksail_revision_capture),
                "KSAIL_COMMAND_CAPTURE": str(self.ksail_command_capture),
                "KSAIL_CONFIG_PATH_CAPTURE": str(self.ksail_config_path_capture),
                "KSAIL_REGISTRY_CAPTURE": str(self.ksail_registry_capture),
                "KSAIL_REGISTRY_OVERRIDE_CAPTURE": str(
                    self.ksail_registry_override_capture
                ),
                "EXPECTED_PULL_USERNAME": expected_username,
                "EXPECTED_PULL_TOKEN": expected_token,
                "EXPECTED_GHCR_REVISION": self._expected_revision(),
                "EXPECTED_KSAIL_TARGET_IMAGE": (
                    "ghcr.io/devantler-tech/ksail:"
                    f"v{KSAIL_OPERATOR_VERSION}"
                ),
                "FAKE_SYNC_STATE_DIR": str(self.sync_state_dir),
                "FLUX_GHCR_SYNC_ATTEMPTS": "2",
                "FLUX_GHCR_SYNC_INTERVAL": "0",
            }
        )
        environment.update(environment_overrides)
        return subprocess.run(
            [str(HELPER), *helper_args],
            cwd=ROOT,
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )

    def _run_ksail_pull_wrapper(
        self,
        config: object,
        command: tuple[str, ...],
        **environment_overrides: str,
    ) -> subprocess.CompletedProcess[str]:
        """Run a production KSail command through the SOPS pull-auth wrapper."""
        self.decrypted_config.write_text(json.dumps(config), encoding="utf-8")
        for marker in (
            self.output_path_log,
            self.ksail_token_capture,
            self.ksail_username_capture,
            self.ksail_revision_capture,
            self.ksail_command_capture,
            self.ksail_config_path_capture,
            self.ksail_registry_capture,
            self.ksail_registry_override_capture,
        ):
            marker.unlink(missing_ok=True)
        expected_username, expected_token = self._expected_credentials(config)
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": f"{self.bin_dir}:{environment['PATH']}",
                "FAKE_DECRYPTED_CONFIG": str(self.decrypted_config),
                "FLUX_GHCR_SECRET_FILE": str(self.encrypted_secret),
                "KSAIL_OUTPUT_PATH_LOG": str(self.output_path_log),
                "KSAIL_TOKEN_CAPTURE": str(self.ksail_token_capture),
                "KSAIL_USERNAME_CAPTURE": str(self.ksail_username_capture),
                "KSAIL_REVISION_CAPTURE": str(self.ksail_revision_capture),
                "KSAIL_COMMAND_CAPTURE": str(self.ksail_command_capture),
                "KSAIL_CONFIG_PATH_CAPTURE": str(self.ksail_config_path_capture),
                "KSAIL_REGISTRY_CAPTURE": str(self.ksail_registry_capture),
                "KSAIL_REGISTRY_OVERRIDE_CAPTURE": str(
                    self.ksail_registry_override_capture
                ),
                "EXPECTED_PULL_USERNAME": expected_username,
                "EXPECTED_PULL_TOKEN": expected_token,
                "EXPECTED_GHCR_REVISION": self._expected_revision(),
            }
        )
        environment.update(environment_overrides)
        return subprocess.run(
            [str(KSAIL_PULL_WRAPPER), *command],
            cwd=ROOT,
            env=environment,
            text=True,
            capture_output=True,
            check=False,
        )

    @staticmethod
    def _valid_config() -> dict[str, object]:
        """Return a valid explicit GHCR Docker authentication fixture."""
        return {
            "auths": {
                "ghcr.io": {
                    "username": "devantler",
                    "password": "fixture-secret-token",
                }
            }
        }

    def test_refreshes_root_and_fanout_without_leaking_plaintext(self) -> None:
        """Refresh every credential consumer without exposing the pull token."""
        config = self._valid_config()

        result = self._run_helper(config)

        self.assertEqual(result.returncode, 0, result.stderr)
        patch = json.loads(self.patch_capture.read_text(encoding="utf-8"))
        encoded = patch["data"][".dockerconfigjson"]

        self.assertEqual(json.loads(base64.b64decode(encoded)), config)
        variables_patch = json.loads(
            self.variables_patch_capture.read_text(encoding="utf-8")
        )
        variables_encoded = variables_patch["data"]["ghcr_dockerconfigjson"]
        self.assertEqual(json.loads(base64.b64decode(variables_encoded)), config)
        temporary_config = Path(self.output_path_log.read_text(encoding="utf-8"))
        self.assertFalse(temporary_config.exists())
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)
        self.assertEqual(
            self.registry_read_log.read_text(encoding="utf-8").splitlines(),
            [
                "devantler-tech/platform/manifests:latest",
                "devantler-tech/wedding-app/manifests:latest",
                "devantler-tech/ascoachingogvaner/manifests:latest",
                "devantler-tech/wedding-app:latest",
                "devantler-tech/ascoachingogvaner:latest",
                f"devantler-tech/ksail:v{KSAIL_OPERATOR_VERSION}",
                "devantler-tech/provider-upjet-unifi:v0.1.0",
            ],
        )
        self.assertEqual(
            self.fanout_log.read_text(encoding="utf-8").splitlines(),
            [
                "pushsecret/flux-system/seed-ghcr",
                "externalsecret/wedding-app/ghcr-auth",
                "externalsecret/ascoachingogvaner/ghcr-auth",
                "externalsecret/kyverno/ghcr-auth",
            ],
        )

    def test_stages_kubernetes_consumers_before_talos_drains(self) -> None:
        """Verify fanout, then patch, REBOOT, drop cache, and pull workers first.

        The reboot is load-bearing: containerd reads registry auth from its
        static config only at process start, and Talos refuses
        `service cri restart`, so a --mode=no-reboot patch leaves the running
        containerd on the OLD credential. Without the reboot every ghcr.io pull
        on the node keeps failing 403 while the node still reports "verified".
        Removing the cached copy first is what makes the pull prove a real
        registry round-trip rather than a cache hit.
        """
        result = self._run_helper(self._valid_config())

        self.assertEqual(result.returncode, 0, result.stderr)
        expected_talos_operations = [
            "talos-auth:10.0.0.2",
            "talos-reboot:10.0.0.2",
            "talos-remove:10.0.0.2:ghcr.io/devantler-tech/ksail:"
            f"v{KSAIL_OPERATOR_VERSION}",
            "talos-pull:10.0.0.2:ghcr.io/devantler-tech/ksail:"
            f"v{KSAIL_OPERATOR_VERSION}",
            "talos-revision:10.0.0.2",
            "talos-auth:10.0.0.1",
            "talos-reboot:10.0.0.1",
            "talos-remove:10.0.0.1:ghcr.io/devantler-tech/ksail:"
            f"v{KSAIL_OPERATOR_VERSION}",
            "talos-pull:10.0.0.1:ghcr.io/devantler-tech/ksail:"
            f"v{KSAIL_OPERATOR_VERSION}",
            "talos-revision:10.0.0.1",
        ]
        self.assertEqual(
            self.talos_log.read_text(encoding="utf-8").splitlines(),
            expected_talos_operations,
        )
        # Every Kubernetes pull consumer is refreshed and verified before the
        # first drain. Each node then completes its full sequence before the
        # next node is touched, and root Flux auth remains the final mutation.
        self.assertEqual(
            self.operation_log.read_text(encoding="utf-8").splitlines(),
            [
                "variables-patch",
                "fanout:pushsecret/flux-system/seed-ghcr",
                "fanout:externalsecret/wedding-app/ghcr-auth",
                "fanout:externalsecret/ascoachingogvaner/ghcr-auth",
                "fanout:externalsecret/kyverno/ghcr-auth",
                "talos-auth:10.0.0.2",
                "talos-reboot:10.0.0.2",
                "node-ready:prod-worker-1",
                "talos-remove:10.0.0.2:ghcr.io/devantler-tech/ksail:"
                f"v{KSAIL_OPERATOR_VERSION}",
                "talos-pull:10.0.0.2:ghcr.io/devantler-tech/ksail:"
                f"v{KSAIL_OPERATOR_VERSION}",
                "talos-revision:10.0.0.2",
                "talos-auth:10.0.0.1",
                "talos-reboot:10.0.0.1",
                "node-ready:prod-control-plane-1",
                "talos-remove:10.0.0.1:ghcr.io/devantler-tech/ksail:"
                f"v{KSAIL_OPERATOR_VERSION}",
                "talos-pull:10.0.0.1:ghcr.io/devantler-tech/ksail:"
                f"v{KSAIL_OPERATOR_VERSION}",
                "talos-revision:10.0.0.1",
                "root-patch",
            ],
        )
        temporary_patch = Path(
            self.talos_patch_path_log.read_text(encoding="utf-8")
        )
        self.assertFalse(temporary_patch.exists())
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)

    def test_unhealthy_control_plane_blocks_the_control_plane_reboot(self) -> None:
        """Never take a SECOND control plane down — etcd would lose quorum.

        The reboot roll is serial and control planes go last, but a control plane
        that was ALREADY unhealthy before this run makes our reboot the second one
        down. The worker still syncs (it cannot affect quorum); the control-plane
        reboot is refused after the Kubernetes credential fanout is made safe.
        """
        revision = self._expected_revision()
        ready = [{"type": "Ready", "status": "True"}]
        inventory = {
            "items": [
                {
                    "metadata": {"name": "prod-worker-1", "labels": {}},
                    "status": {
                        "addresses": [{"type": "InternalIP", "address": "10.0.0.2"}],
                        "conditions": ready,
                    },
                },
                {
                    "metadata": {
                        "name": "prod-control-plane-1",
                        "labels": {"node-role.kubernetes.io/control-plane": ""},
                    },
                    "status": {
                        "addresses": [{"type": "InternalIP", "address": "10.0.0.1"}],
                        "conditions": ready,
                    },
                },
                {
                    # Already down, and already at the desired revision — so it is
                    # not itself a sync target, it is just a quorum member.
                    "metadata": {
                        "name": "prod-control-plane-2",
                        "labels": {"node-role.kubernetes.io/control-plane": ""},
                        "annotations": {
                            "platform.devantler.tech/ghcr-pull-verified-revision-v2":
                                revision,
                            "platform.devantler.tech/ghcr-pull-verified-image-v2": (
                                "ghcr.io/devantler-tech/ksail:"
                                f"v{KSAIL_OPERATOR_VERSION}"
                            ),
                        },
                    },
                    "status": {
                        "addresses": [{"type": "InternalIP", "address": "10.0.0.3"}],
                        "conditions": [{"type": "Ready", "status": "False"}],
                    },
                },
            ]
        }

        result = self._run_helper(
            self._valid_config(),
            FAKE_NODE_JSON=json.dumps(inventory),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("risks quorum", result.stdout + result.stderr)
        operations = self.operation_log.read_text(encoding="utf-8").splitlines()
        self.assertIn("talos-reboot:10.0.0.2", operations)
        self.assertNotIn("talos-reboot:10.0.0.1", operations)
        self.assertNotIn("root-patch", operations)
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)

    def test_pre_existing_cordon_survives_the_auth_reboot(self) -> None:
        """A node an operator cordoned must not come back schedulable.

        `talosctl reboot --drain` cordons and drains, then UNCORDONS on the way
        out. A node deliberately cordoned before this run (maintenance,
        investigation, autoscaler scale-down) would silently become schedulable
        again just because we rebooted it to refresh a credential.
        """
        result = self._run_helper(
            self._valid_config(),
            FAKE_CORDONED_NODES="prod-worker-1",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        operations = self.operation_log.read_text(encoding="utf-8").splitlines()
        self.assertIn("node-cordon:prod-worker-1", operations)
        # The cordon is restored only after the node is back, and only for the
        # node that was cordoned to begin with.
        self.assertLess(
            operations.index("node-ready:prod-worker-1"),
            operations.index("node-cordon:prod-worker-1"),
        )
        self.assertNotIn("node-cordon:prod-control-plane-1", operations)

    def test_uncordoned_node_is_not_cordoned_after_the_auth_reboot(self) -> None:
        """Never leave a node unschedulable that was schedulable before."""
        result = self._run_helper(self._valid_config())

        self.assertEqual(result.returncode, 0, result.stderr)
        operations = self.operation_log.read_text(encoding="utf-8").splitlines()
        self.assertFalse(
            [entry for entry in operations if entry.startswith("node-cordon:")],
            "a node that was schedulable before the reboot was left cordoned",
        )

    def test_unready_node_after_reboot_stops_the_roll(self) -> None:
        """Never roll the next node while the rebooted one is still down."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_NODE_READY_FAIL_NODE="prod-worker-1",
        )

        self.assertNotEqual(result.returncode, 0)
        operations = self.operation_log.read_text(encoding="utf-8").splitlines()
        # The fanout is already safe. The worker rebooted but never came back,
        # so the control plane is left alone and root Flux auth is unchanged.
        self.assertEqual(
            operations,
            [
                "variables-patch",
                "fanout:pushsecret/flux-system/seed-ghcr",
                "fanout:externalsecret/wedding-app/ghcr-auth",
                "fanout:externalsecret/ascoachingogvaner/ghcr-auth",
                "fanout:externalsecret/kyverno/ghcr-auth",
                "talos-auth:10.0.0.2",
                "talos-reboot:10.0.0.2",
                "node-ready:prod-worker-1",
            ],
        )
        self.assertNotIn("talos-revision:10.0.0.2", operations)
        self.assertNotIn("root-patch", operations)
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)

    def test_talos_failure_after_safe_fanout_keeps_root_auth_unchanged(self) -> None:
        """Keep root Flux auth unchanged when the post-fanout node roll fails."""
        for operation in ("auth", "reboot", "remove", "pull", "revision"):
            with self.subTest(operation=operation):
                result = self._run_helper(
                    self._valid_config(),
                    FAKE_TALOS_FAIL_NODE="10.0.0.2",
                    FAKE_TALOS_FAIL_OPERATION=operation,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(self.variables_patch_capture.exists())
                self.assertFalse(self.patch_capture.exists())
                self.assertEqual(
                    self.fanout_log.read_text(encoding="utf-8").splitlines(),
                    [
                        "pushsecret/flux-system/seed-ghcr",
                        "externalsecret/wedding-app/ghcr-auth",
                        "externalsecret/ascoachingogvaner/ghcr-auth",
                        "externalsecret/kyverno/ghcr-auth",
                    ],
                )
                self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)

    def test_missing_cached_image_still_pulls_and_records_revision(self) -> None:
        """Treat a confirmed absent cache entry as ready for pull proof."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_TALOS_IMAGE_ABSENT_NODE="10.0.0.2",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        target_image = (
            "ghcr.io/devantler-tech/ksail:"
            f"v{KSAIL_OPERATOR_VERSION}"
        )
        operations = self.talos_log.read_text(encoding="utf-8").splitlines()
        self.assertIn(f"talos-remove:10.0.0.2:{target_image}", operations)
        self.assertIn(f"talos-pull:10.0.0.2:{target_image}", operations)
        self.assertIn("talos-revision:10.0.0.2", operations)
        self.assertTrue(self.patch_capture.exists())
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)

    def test_current_talos_nodes_skip_talos_api(self) -> None:
        """Skip Talos only after this revision and target image are proved."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_TALOS_NODES_CURRENT="true",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.talos_log.exists())
        self.assertTrue(self.patch_capture.exists())

    def test_matching_revision_revalidates_changed_declared_image(self) -> None:
        """Do not trust a current credential marker for a new target image."""
        previous_image = "ghcr.io/devantler-tech/ksail:v7.166.0"
        target_image = (
            "ghcr.io/devantler-tech/ksail:"
            f"v{KSAIL_OPERATOR_VERSION}"
        )

        result = self._run_helper(
            self._valid_config(),
            FAKE_TALOS_NODES_CURRENT="true",
            FAKE_TALOS_VERIFIED_IMAGE=previous_image,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(
            self.talos_log.exists(),
            "matching credential revision incorrectly skipped target-image proof",
        )
        operations = self.talos_log.read_text(encoding="utf-8").splitlines()
        self.assertEqual(
            operations,
            [
                "talos-auth:10.0.0.2",
                "talos-reboot:10.0.0.2",
                f"talos-remove:10.0.0.2:{target_image}",
                f"talos-pull:10.0.0.2:{target_image}",
                "talos-revision:10.0.0.2",
                "talos-auth:10.0.0.1",
                "talos-reboot:10.0.0.1",
                f"talos-remove:10.0.0.1:{target_image}",
                f"talos-pull:10.0.0.1:{target_image}",
                "talos-revision:10.0.0.1",
            ],
        )
        self.assertNotIn(previous_image, "\n".join(operations))

    def test_dr_without_fanout_does_not_drain_nodes(self) -> None:
        """Never drain workloads before a DR cluster has its pull fanout."""
        result = self._run_helper(
            self._valid_config(),
            ("--allow-incomplete-fanout",),
            FAKE_VARIABLES_BASE_ABSENT="true",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.talos_log.exists())
        self.assertTrue(self.patch_capture.exists())

    def test_invalid_node_inventory_fails_closed(self) -> None:
        """Reject empty, duplicate, and ambiguous Talos node InternalIPs."""
        invalid_inventories = [
            {"items": []},
            {
                "items": [
                    {
                        "metadata": {"name": "one"},
                        "status": {"addresses": []},
                    }
                ]
            },
            {
                "items": [
                    {
                        "metadata": {"name": "one"},
                        "status": {
                            "addresses": [
                                {"type": "InternalIP", "address": "10.0.0.1"},
                                {"type": "InternalIP", "address": "10.0.0.2"},
                            ]
                        },
                    }
                ]
            },
            {
                "items": [
                    {
                        "metadata": {"name": "one"},
                        "status": {"addresses": [
                            {"type": "InternalIP", "address": "10.0.0.1"}
                        ]},
                    },
                    {
                        "metadata": {"name": "two"},
                        "status": {"addresses": [
                            {"type": "InternalIP", "address": "10.0.0.1"}
                        ]},
                    },
                ]
            },
        ]
        for inventory in invalid_inventories:
            with self.subTest(inventory=inventory):
                result = self._run_helper(
                    self._valid_config(),
                    FAKE_NODE_JSON=json.dumps(inventory),
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.talos_log.exists())
                self.assertFalse(self.patch_capture.exists())

    def test_node_discovery_failure_after_safe_fanout_keeps_root_unchanged(
        self,
    ) -> None:
        """Fail closed after safe fanout when production nodes cannot be listed."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_NODE_DISCOVERY_FAIL="true",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.talos_log.exists())
        self.assertTrue(self.variables_patch_capture.exists())
        self.assertTrue(self.fanout_log.exists())
        self.assertFalse(self.patch_capture.exists())

    def test_ksail_lifecycle_wrapper_uses_only_sops_pull_token(self) -> None:
        """Run create, reconcile, and update with the decrypted pull token."""
        self.assertTrue(KSAIL_PULL_WRAPPER.is_file())
        for command in (
            ("cluster", "create"),
            ("workload", "reconcile"),
            ("cluster", "update"),
        ):
            with self.subTest(command=command):
                result = self._run_ksail_pull_wrapper(self._valid_config(), command)

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(
                    self.ksail_token_capture.read_text(encoding="utf-8"),
                    "fixture-secret-token",
                )
                self.assertEqual(
                    self.ksail_username_capture.read_text(encoding="utf-8"),
                    "devantler",
                )
                self.assertEqual(
                    self.ksail_command_capture.read_text(encoding="utf-8")
                    .strip()
                    .split(),
                    ["--config", "ksail.prod.yaml", *command],
                )
                self.assertEqual(
                    self.ksail_config_path_capture.read_text(encoding="utf-8"),
                    "ksail.prod.yaml",
                )
                self.assertEqual(
                    self.ksail_registry_capture.read_text(encoding="utf-8"),
                    "devantler:${GHCR_TOKEN}"
                    "@ghcr.io/devantler-tech/platform/manifests",
                )
                self.assertEqual(
                    self.ksail_registry_override_capture.read_text(
                        encoding="utf-8"
                    ),
                    "${GHCR_USERNAME}:${GHCR_TOKEN}"
                    "@ghcr.io/devantler-tech/platform/manifests",
                )
                self.assertRegex(
                    self.ksail_revision_capture.read_text(encoding="utf-8"),
                    r"^[0-9a-f]{64}$",
                )
                self.assertNotIn(
                    "fixture-secret-token",
                    result.stdout + result.stderr,
                )
                temporary_config = Path(
                    self.output_path_log.read_text(encoding="utf-8")
                )
                self.assertFalse(temporary_config.exists())

    def test_ksail_publish_wrapper_preserves_actions_write_token(self) -> None:
        """Inject only the SOPS revision while preserving publication auth."""
        result = self._run_ksail_pull_wrapper(
            self._valid_config(),
            ("workload", "push"),
            GITHUB_ACTOR="fixture-publisher",
            GHCR_TOKEN="fixture-actions-write-token",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            self.ksail_token_capture.read_text(encoding="utf-8"),
            "fixture-actions-write-token",
        )
        self.assertEqual(
            self.ksail_username_capture.read_text(encoding="utf-8"),
            "fixture-publisher",
        )
        self.assertEqual(
            self.ksail_config_path_capture.read_text(encoding="utf-8"),
            "ksail.prod.yaml",
        )
        self.assertEqual(
            self.ksail_registry_capture.read_text(encoding="utf-8"),
            "devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests",
        )
        self.assertEqual(
            self.ksail_registry_override_capture.read_text(encoding="utf-8"),
            "",
        )
        self.assertRegex(
            self.ksail_revision_capture.read_text(encoding="utf-8"),
            r"^[0-9a-f]{64}$",
        )
        self.assertNotIn(
            "fixture-actions-write-token",
            result.stdout + result.stderr,
        )
        self.assertFalse(
            self.output_path_log.exists(),
            "publication needs the ciphertext revision but not decryption",
        )

    def test_production_config_keeps_its_protected_registry_template(self) -> None:
        """Keep dynamic pull auth in the wrapper-owned environment only."""
        config = KSAIL_PROD_CONFIG.read_text(encoding="utf-8")

        self.assertIn(
            'registry: "devantler:${GHCR_TOKEN}'
            '@ghcr.io/devantler-tech/platform/manifests"',
            config,
        )
        self.assertNotIn("${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io", config)

    def test_lifecycle_preserves_username_from_sops_docker_config(self) -> None:
        """Avoid hard-coding the pull credential's registry username."""
        config = self._valid_config()
        config["auths"]["ghcr.io"]["username"] = "pull-robot"  # type: ignore[index]

        result = self._run_ksail_pull_wrapper(
            config,
            ("cluster", "update"),
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            self.ksail_username_capture.read_text(encoding="utf-8"),
            "pull-robot",
        )

    def test_ciphertext_rotation_changes_revision_without_hashing_token(self) -> None:
        """Drive template drift from SOPS ciphertext rather than plaintext auth."""
        first = self._run_ksail_pull_wrapper(
            self._valid_config(),
            ("cluster", "update"),
        )
        self.assertEqual(first.returncode, 0, first.stderr)
        first_revision = self.ksail_revision_capture.read_text(encoding="utf-8")

        self._write_encrypted_secret("ENC[AES256_GCM,data:fixture-two]")
        second = self._run_ksail_pull_wrapper(
            self._valid_config(),
            ("cluster", "update"),
        )
        self.assertEqual(second.returncode, 0, second.stderr)
        second_revision = self.ksail_revision_capture.read_text(encoding="utf-8")

        normalized_plaintext = json.dumps(
            self._valid_config(), sort_keys=True, separators=(",", ":")
        )
        plaintext_hash = hashlib.sha256(
            f"{normalized_plaintext}\n".encode()
        ).hexdigest()
        self.assertNotEqual(first_revision, second_revision)
        self.assertNotEqual(first_revision, plaintext_hash)
        self.assertNotEqual(second_revision, plaintext_hash)

    def test_wrapper_rejects_arbitrary_commands(self) -> None:
        """Never pass the decrypted pull credential to an arbitrary process."""
        result = self._run_ksail_pull_wrapper(
            self._valid_config(),
            ("workload", "delete"),
        )

        self.assertEqual(result.returncode, 64)
        self.assertFalse(self.ksail_token_capture.exists())

    def test_plaintext_revision_source_fails_closed(self) -> None:
        """Refuse to hash a source value that is not SOPS ciphertext."""
        self._write_encrypted_secret("accidentally-plaintext")

        result = self._run_ksail_pull_wrapper(
            self._valid_config(),
            ("cluster", "update"),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.ksail_token_capture.exists())

    def test_accepts_standard_auth_only_docker_config(self) -> None:
        """Accept Docker configs that store only the standard encoded auth field."""
        auth = base64.b64encode(b"devantler:fixture-secret-token").decode()
        config = {"auths": {"ghcr.io": {"auth": auth}}}

        result = self._run_helper(config)

        self.assertEqual(result.returncode, 0, result.stderr)
        patch = json.loads(self.patch_capture.read_text(encoding="utf-8"))
        encoded = patch["data"][".dockerconfigjson"]
        self.assertEqual(json.loads(base64.b64decode(encoded)), config)

    def test_accepts_matching_explicit_and_encoded_auth(self) -> None:
        """Accept matching explicit and encoded GHCR credentials."""
        config = self._valid_config()
        registry_auth = config["auths"]["ghcr.io"]
        registry_auth["auth"] = base64.b64encode(
            b"devantler:fixture-secret-token"
        ).decode()

        result = self._run_helper(config)

        self.assertEqual(result.returncode, 0, result.stderr)

    def test_check_only_preflights_without_patching(self) -> None:
        """Keep registry preflight mode free of Kubernetes mutations."""
        result = self._run_helper(self._valid_config(), ("--check-only",))

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.kubectl_called.exists())
        self.assertFalse(self.patch_capture.exists())

    def test_missing_or_malformed_registry_auth_fails_closed(self) -> None:
        """Reject missing, malformed, empty, or contradictory GHCR credentials."""
        invalid_configs: list[object] = [
            {"auths": {"ghcr.io": {"username": "devantler"}}},
            {"auths": {"ghcr.io": {"username": "", "password": "token"}}},
            {"auths": {"ghcr.io": {"auth": "not-base64"}}},
            {
                "auths": {
                    "ghcr.io": {
                        "username": "devantler",
                        "password": "fixture-secret-token",
                        "auth": base64.b64encode(
                            b"devantler:different-token"
                        ).decode(),
                    }
                }
            },
            {"auths": {}},
            "not-a-docker-config",
        ]

        for config in invalid_configs:
            with self.subTest(config=config):
                result = self._run_helper(config)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.kubectl_called.exists())

    def test_registry_denial_prevents_cluster_patch(self) -> None:
        """Leave cluster credentials untouched when GHCR denies a manifest read."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_CURL_DENY_REPOSITORY="devantler-tech/platform/manifests",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.kubectl_called.exists())

    def test_token_success_without_registry_read_access_prevents_cluster_patch(
        self,
    ) -> None:
        """Require package read access even when the token exchange succeeds."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_CURL_DENY_REPOSITORY="devantler-tech/wedding-app",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.kubectl_called.exists())

    def test_cluster_patch_failure_is_not_hidden(self) -> None:
        """Propagate a failure to patch the root Flux pull Secret."""
        result = self._run_helper(self._valid_config(), FAKE_KUBECTL_FAIL="true")

        self.assertEqual(result.returncode, 43)
        self.assertTrue(self.kubectl_called.exists())

    def test_fresh_cluster_without_variables_base_skips_existing_fanout(self) -> None:
        """Bootstrap only root auth before a fresh cluster creates its fan-out."""
        result = self._run_helper(
            self._valid_config(),
            ("--allow-incomplete-fanout",),
            FAKE_VARIABLES_BASE_ABSENT="true",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.patch_capture.exists())
        self.assertFalse(self.variables_patch_capture.exists())
        self.assertFalse(self.fanout_log.exists())

    def test_missing_variables_base_fails_closed_without_bootstrap_mode(self) -> None:
        """Require the explicit DR flag when the credential fan-out is absent."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_VARIABLES_BASE_ABSENT="true",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.patch_capture.exists())

    def test_partial_bootstrap_repairs_root_without_forcing_missing_fanout(
        self,
    ) -> None:
        """Stage DR root auth without force-syncing an incomplete fan-out."""
        missing_resources = [
            "pushsecret/flux-system/seed-ghcr",
            "externalsecret/wedding-app/ghcr-auth",
            "externalsecret/ascoachingogvaner/ghcr-auth",
            "externalsecret/kyverno/ghcr-auth",
        ]
        for resource in missing_resources:
            with self.subTest(resource=resource):
                result = self._run_helper(
                    self._valid_config(),
                    ("--allow-incomplete-fanout",),
                    FAKE_MISSING_FANOUT_RESOURCE=resource,
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(self.variables_patch_capture.exists())
                self.assertTrue(self.patch_capture.exists())
                self.assertFalse(self.fanout_log.exists())
                self.assertIn("first reconcile will complete", result.stdout)
                self.assertEqual(
                    self.operation_log.read_text(encoding="utf-8").splitlines()[-3:],
                    ["root-patch", "variables-patch", "root-patch"],
                )

    def test_partial_bootstrap_repairs_root_before_staging_variables(self) -> None:
        """Keep an unavailable root patch from advancing partial consumers."""
        result = self._run_helper(
            self._valid_config(),
            ("--allow-incomplete-fanout",),
            FAKE_MISSING_FANOUT_RESOURCE="pushsecret/flux-system/seed-ghcr",
            FAKE_KUBECTL_FAIL="true",
        )

        self.assertEqual(result.returncode, 43)
        self.assertFalse(self.variables_patch_capture.exists())

    def test_partial_fanout_fails_closed_without_bootstrap_mode(self) -> None:
        """Reject a partial fan-out during a normal production deployment."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_MISSING_FANOUT_RESOURCE="externalsecret/kyverno/ghcr-auth",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.variables_patch_capture.exists())
        self.assertFalse(self.patch_capture.exists())
        self.assertFalse(self.fanout_log.exists())

    def test_missing_eso_crds_fails_without_staging_variables(self) -> None:
        """Leave the fan-out source unchanged when normal mode is incomplete."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_FANOUT_CRDS_ABSENT="true",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.variables_patch_capture.exists())
        self.assertFalse(self.patch_capture.exists())

    def test_partial_bootstrap_without_eso_crds_repairs_root(self) -> None:
        """Permit DR root repair before External Secrets CRDs exist."""
        result = self._run_helper(
            self._valid_config(),
            ("--allow-incomplete-fanout",),
            FAKE_FANOUT_CRDS_ABSENT="true",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.variables_patch_capture.exists())
        self.assertTrue(self.patch_capture.exists())
        self.assertFalse(self.fanout_log.exists())

    def test_pushsecret_sync_failure_is_not_hidden(self) -> None:
        """Keep root auth unchanged when the OpenBao seed does not reconcile."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_SYNC_STALL_RESOURCE="pushsecret/flux-system/seed-ghcr",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(
            self.patch_capture.exists(),
            "root Flux auth must remain unchanged until fan-out verifies",
        )
        self.assertTrue(self.variables_patch_capture.exists())
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)

    def test_same_second_sync_accepts_controller_resource_version_edge(self) -> None:
        """Accept a controller update when refreshTime has one-second precision."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_SYNC_SAME_REFRESH_TIME="true",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.patch_capture.exists())

    def test_materialised_consumer_mismatch_is_not_hidden(self) -> None:
        """Reject fan-out completion when a workload Secret has stale content."""
        result = self._run_helper(
            self._valid_config(),
            FAKE_CONSUMER_MISMATCH_NAMESPACE="wedding-app",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn(
            "wedding-app/ghcr-auth did not materialise",
            result.stdout + result.stderr,
        )
        self.assertFalse(
            self.patch_capture.exists(),
            "root Flux auth must remain unchanged until consumers match",
        )
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)


class DeployActionOrderingTests(unittest.TestCase):
    """Keep credential refreshes on both sides of KSail-owned mutations."""

    def test_cluster_lifecycle_uses_sops_auth_but_publish_keeps_actions_token(
        self,
    ) -> None:
        """Separate KSail pull lifecycle auth from the Actions publish token."""
        action = ACTION.read_text(encoding="utf-8")
        workflow = DR_REBUILD.read_text(encoding="utf-8")
        wrapper = "./scripts/run-ksail-prod-with-pull-auth.sh"

        action_reconcile = action.index("id: reconcile")
        action_update = action.index("name: 🔄 Update cluster")
        action_reassert = action.index("id: reassert_flux_ghcr_auth")
        self.assertIn(
            f"run: {wrapper} workload reconcile",
            action[action_reconcile:action_update],
        )
        self.assertNotIn("GHCR_TOKEN:", action[action_reconcile:action_update])
        self.assertIn(
            f"run: {wrapper} cluster update",
            action[action_update:action_reassert],
        )
        self.assertNotIn("GHCR_TOKEN:", action[action_update:action_reassert])

        action_push = action.index("name: 📦 Push manifests to GHCR")
        action_sign = action.index("name: ⚙️ Install cosign")
        self.assertIn(
            f"run: {wrapper} workload push",
            action[action_push:action_sign],
        )
        self.assertIn(
            "GHCR_TOKEN: ${{ inputs.ghcr-token }}",
            action[action_push:action_sign],
        )

        dr_create = workflow.index("name: 🏗️ Create cluster")
        dr_stage = workflow.index("id: stage_flux_ghcr_auth")
        self.assertIn(
            f"run: {wrapper} cluster create",
            workflow[dr_create:dr_stage],
        )
        self.assertNotIn("GHCR_TOKEN:", workflow[dr_create:dr_stage])

        dr_push = workflow.index("name: 📦 Push manifests to GHCR")
        dr_verify = workflow.index("id: verify_flux_ghcr_auth_after_push")
        self.assertIn(
            f"run: {wrapper} workload push",
            workflow[dr_push:dr_verify],
        )
        self.assertIn(
            "GHCR_TOKEN: ${{ secrets.GHCR_TOKEN }}",
            workflow[dr_push:dr_verify],
        )

        dr_reconcile = workflow.index("name: 🔁 Trigger Flux reconciliation")
        dr_wait = workflow.index("name: ⏳ Wait for Flux to settle")
        self.assertIn(
            f"run: {wrapper} workload reconcile",
            workflow[dr_reconcile:dr_wait],
        )
        self.assertNotIn("GHCR_TOKEN:", workflow[dr_reconcile:dr_wait])

    def test_talosctl_is_installed_before_any_mutating_bridge(self) -> None:
        """Ensure both isolated Actions jobs can execute node synchronization."""
        action = ACTION.read_text(encoding="utf-8")
        workflow = DR_REBUILD.read_text(encoding="utf-8")

        for document in (action, workflow):
            with self.subTest(document=document[:40]):
                setup = document.index("name: ⚙️ Setup talosctl")
                stage = document.index("id: stage_flux_ghcr_auth")
                self.assertLess(setup, stage)
                setup_step = document[setup:stage]
                self.assertIn(
                    f'TALOS_VERSION: "{KSAIL_PROD_TALOS_VERSION}"', setup_step
                )
                self.assertIn("talosctl-linux-amd64", setup_step)
                if document == action:
                    restore = document.index("name: 🔑 Restore talosconfig")
                    self.assertLess(restore, stage)

    def test_consumer_staging_precedes_publish_and_is_reasserted_after_update(
        self,
    ) -> None:
        """Keep full credential staging before publish and after cluster update."""
        action = ACTION.read_text(encoding="utf-8")
        wrapper = "./scripts/run-ksail-prod-with-pull-auth.sh"

        first_refresh = action.index("id: stage_flux_ghcr_auth")
        push = action.index(f"run: {wrapper} workload push")
        post_push_refresh = action.index("id: verify_flux_ghcr_auth_after_push")
        reconcile = action.index("id: reconcile")
        cluster_update = action.index(f"run: {wrapper} cluster update")
        final_refresh = action.index("id: reassert_flux_ghcr_auth\n")

        self.assertLess(first_refresh, push)
        self.assertLess(push, post_push_refresh)
        self.assertLess(post_push_refresh, reconcile)
        self.assertLess(reconcile, cluster_update)
        self.assertLess(cluster_update, final_refresh)
        self.assertIn(
            "run: ./scripts/refresh-flux-ghcr-auth.sh\n",
            action[first_refresh:push],
        )
        self.assertNotIn(
            "--check-only",
            action[first_refresh:push],
        )
        self.assertIn(
            "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only",
            action[post_push_refresh:reconcile],
        )
        final_refresh_step = action[final_refresh:]
        self.assertIn("always() &&", final_refresh_step)
        self.assertIn(
            "steps.verify_flux_ghcr_auth_after_push.outcome == 'success'",
            final_refresh_step,
        )
        self.assertIn(
            "steps.reconcile.outcome == 'success'",
            final_refresh_step,
        )
        self.assertEqual(action.count("scripts/refresh-flux-ghcr-auth.sh"), 3)

    def test_disaster_rebuild_preflights_then_stages_before_publish(
        self,
    ) -> None:
        """Keep DR credential checks around publish and OpenBao restoration."""
        workflow = DR_REBUILD.read_text(encoding="utf-8")
        wrapper = "./scripts/run-ksail-prod-with-pull-auth.sh"

        preflight = workflow.index(
            "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only"
        )
        cluster_create = workflow.index(f"run: {wrapper} cluster create")
        stage = workflow.index("id: stage_flux_ghcr_auth")
        push = workflow.index(f"run: {wrapper} workload push")
        verify = workflow.index("id: verify_flux_ghcr_auth_after_push")
        fanout_verify = workflow.index("id: verify_flux_ghcr_fanout")
        openbao_restore = workflow.index(
            "name: 🔐 Restore OpenBao from the R2 snapshot mirror"
        )
        post_restore_verify = workflow.index(
            "id: reassert_flux_ghcr_after_restore"
        )
        reconcile = workflow.index(f"run: {wrapper} workload reconcile")

        self.assertLess(preflight, cluster_create)
        self.assertLess(cluster_create, stage)
        self.assertLess(stage, push)
        self.assertLess(push, verify)
        self.assertLess(verify, reconcile)
        self.assertLess(reconcile, fanout_verify)
        self.assertLess(fanout_verify, openbao_restore)
        self.assertLess(openbao_restore, post_restore_verify)
        self.assertIn(
            "run: ./scripts/refresh-flux-ghcr-auth.sh --allow-incomplete-fanout",
            workflow[stage:push],
        )
        self.assertIn(
            "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only",
            workflow[verify:reconcile],
        )
        self.assertIn(
            "run: ./scripts/refresh-flux-ghcr-auth.sh\n",
            workflow[fanout_verify:],
        )
        self.assertIn(
            "if: ${{ !cancelled() && inputs.restore && "
            "steps.verify_flux_ghcr_fanout.outcome == 'success' }}",
            workflow[post_restore_verify:],
        )
        self.assertEqual(workflow.count("scripts/refresh-flux-ghcr-auth.sh"), 5)

    def test_manual_dr_waits_for_flux_before_full_bridge(self) -> None:
        """Keep bootstrap mode until every first-reconcile layer is Ready."""
        runbook = DR_RUNBOOK.read_text(encoding="utf-8")
        ci_workflow = CI_WORKFLOW.read_text(encoding="utf-8")
        manual = runbook[
            runbook.index("# 2. Prove the Git/SOPS pull credential") :
            runbook.index("# 6. ONLY if the OpenBao raft-snapshot")
        ]

        bootstrap = manual.index(
            "./scripts/refresh-flux-ghcr-auth.sh --allow-incomplete-fanout"
        )
        reconcile = manual.index(
            "./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile"
        )
        wait = manual.index(
            "kubectl --context admin@prod -n flux-system wait"
        )
        full_bridge = manual.index(
            "./scripts/refresh-flux-ghcr-auth.sh  # prove completed fan-out"
        )

        self.assertLess(bootstrap, reconcile)
        self.assertLess(reconcile, wait)
        self.assertLess(wait, full_bridge)
        self.assertIn(
            "workload reconciliation also requires the SOPS key",
            runbook,
        )
        self.assertIn("- 'docs/dr/runbook.md'", ci_workflow)

    def test_bridge_docs_and_tests_validate_without_deploying(self) -> None:
        """Keep validation-only bridge changes out of production deploys."""
        workflow = CI_WORKFLOW.read_text(encoding="utf-8")
        filters = workflow[
            workflow.index("          filters: |") : workflow.index("\n  validate:")
        ]
        k8s_filter = filters[
            filters.index("            k8s:") : filters.index(
                "            bridge_validation:"
            )
        ]
        bridge_filter = filters[
            filters.index("            bridge_validation:") : filters.index(
                "            talos:"
            )
        ]

        for validation_only_path in (
            "scripts/tests/test-refresh-flux-ghcr-auth-safety.sh",
            "scripts/tests/test_refresh_flux_ghcr_auth.py",
            "docs/dr/runbook.md",
        ):
            with self.subTest(path=validation_only_path):
                quoted_path = f"- '{validation_only_path}'"
                self.assertNotIn(quoted_path, k8s_filter)
                self.assertIn(quoted_path, bridge_filter)

        self.assertIn(
            "bridge_validation: ${{ steps.filter.outputs.bridge_validation }}",
            workflow,
        )
        validate = workflow[
            workflow.index("  validate:") : workflow.index("  naming:")
        ]
        self.assertIn(
            "needs.changes.outputs.bridge_validation == 'true'", validate
        )
        deploy = workflow[
            workflow.index("  deploy-prod:") : workflow.index(
                "  heal-prod-on-failure:"
            )
        ]
        heal = workflow[
            workflow.index("  heal-prod-on-failure:") : workflow.index(
                "  ci-required-checks:"
            )
        ]
        for production_job in (deploy, heal):
            self.assertIn("needs.changes.outputs.k8s == 'true'", production_job)
            self.assertNotIn("bridge_validation", production_job)


class ManualBridgePrerequisiteTests(unittest.TestCase):
    """Keep manual recovery dependencies explicit and fail-fast."""

    def test_yq_v4_is_documented_and_preflighted(self) -> None:
        """Require the YAML tool used before any GHCR recovery mutation."""
        instructions = AGENT_INSTRUCTIONS.read_text(encoding="utf-8")
        runbook = DR_RUNBOOK.read_text(encoding="utf-8")
        library = GHCR_AUTH_LIB.read_text(encoding="utf-8")

        self.assertIn("yq v4", instructions)
        self.assertIn("yq v4", runbook[: runbook.index("## Scenario 1")])
        self.assertIn("require_flux_ghcr_yaml_tool()", library)
        for entrypoint in (HELPER, KSAIL_PULL_WRAPPER):
            with self.subTest(entrypoint=entrypoint.name):
                self.assertIn(
                    "require_flux_ghcr_yaml_tool",
                    entrypoint.read_text(encoding="utf-8"),
                )

    def test_yaml_tool_preflight_rejects_missing_or_incompatible_yq(self) -> None:
        """Stop before entrypoint work when Mike Farah yq v4 is unavailable."""
        cases = ("missing", "incompatible")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as temp_dir:
                if case == "incompatible":
                    fake_yq = Path(temp_dir) / "yq"
                    fake_yq.write_text(
                        "#!/bin/bash\necho 'yq 3.4.3'\n", encoding="utf-8"
                    )
                    fake_yq.chmod(0o755)
                environment = os.environ.copy()
                environment["PATH"] = temp_dir

                result = subprocess.run(
                    [
                        "/bin/bash",
                        "-c",
                        'source "$1"; require_flux_ghcr_yaml_tool',
                        "bash",
                        str(GHCR_AUTH_LIB),
                    ],
                    env=environment,
                    text=True,
                    capture_output=True,
                    check=False,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("yq v4 is required", result.stderr)


class RequiredPackageCoverageTests(unittest.TestCase):
    """Keep pinned private provider references in the live GHCR preflight."""

    def test_provider_upjet_unifi_reference_is_preflighted(self) -> None:
        """Require the live private provider package in the GHCR preflight."""
        manifest = PROVIDER_UPJET_UNIFI.read_text(encoding="utf-8")
        helper = HELPER.read_text(encoding="utf-8")

        package_line = next(
            line.strip()
            for line in manifest.splitlines()
            if line.strip().startswith("package: ghcr.io/")
        )
        package_reference = package_line.removeprefix("package: ghcr.io/")

        self.assertIn(f'"{package_reference}"', helper)

    def test_exact_declared_ksail_operator_image_is_preflighted(self) -> None:
        """Bind the GHCR preflight to the chart's exact KSail image version."""
        helper = HELPER.read_text(encoding="utf-8")

        self.assertIn(
            '"devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"', helper
        )
        self.assertIn(".spec.chart.spec.version", helper)
        self.assertTrue(KSAIL_OPERATOR_VERSION)


class TalosRegistryAuthConfigTests(unittest.TestCase):
    """Keep lifecycle drift and post-pull verification states distinct."""

    def test_static_desired_revision_cannot_claim_verified_pull(self) -> None:
        """Require helper proof before a new Talos node is considered current."""
        static_revision = TALOS_GHCR_REVISION.read_text(encoding="utf-8")
        static_config = "\n".join(
            line
            for line in static_revision.splitlines()
            if not line.lstrip().startswith("#")
        )
        helper = HELPER.read_text(encoding="utf-8")

        self.assertIn("ghcr-pull-desired-revision", static_config)
        self.assertNotIn("ghcr-pull-verified-revision", static_config)
        self.assertIn("ghcr-pull-verified-revision", helper)

    def test_registry_auth_uses_supported_talos_document(self) -> None:
        """Use one standalone RegistryAuthConfig document with variable auth."""
        registry_auth = TALOS_GHCR_AUTH.read_text(encoding="utf-8")

        self.assertIn("kind: RegistryAuthConfig", registry_auth)
        self.assertIn("username: ${GHCR_USERNAME}", registry_auth)
        self.assertIn("password: ${GHCR_TOKEN}", registry_auth)
        self.assertNotIn("machine:\n", registry_auth)
        self.assertNotIn("\n---\n", registry_auth)


if __name__ == "__main__":
    unittest.main()
