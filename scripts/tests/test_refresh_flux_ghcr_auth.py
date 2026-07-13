"""Regression tests for the production Flux GHCR credential bridge."""

from __future__ import annotations

import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "scripts" / "refresh-flux-ghcr-auth.sh"
ACTION = ROOT / ".github" / "actions" / "deploy-prod" / "action.yml"
DR_REBUILD = ROOT / ".github" / "workflows" / "dr-rebuild.yaml"
PROVIDER_UPJET_UNIFI = (
    ROOT
    / "k8s"
    / "providers"
    / "hetzner"
    / "infrastructure"
    / "crossplane"
    / "provider-upjet-unifi.yaml"
)


class RefreshFluxGhcrAuthTests(unittest.TestCase):
    """Exercise the helper with fake external commands and no real secrets."""

    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp_dir.cleanup)
        self.workspace = Path(self.temp_dir.name)
        self.bin_dir = self.workspace / "bin"
        self.bin_dir.mkdir()
        self.decrypted_config = self.workspace / "decrypted-config.json"
        self.patch_capture = self.workspace / "patch.json"
        self.variables_patch_capture = self.workspace / "variables-patch.json"
        self.kubectl_called = self.workspace / "kubectl-called"
        self.output_path_log = self.workspace / "ksail-output-path"
        self.registry_read_log = self.workspace / "registry-reads"
        self.fanout_log = self.workspace / "fanout-log"
        self.sync_state_dir = self.workspace / "sync-state"
        self.sync_state_dir.mkdir()

        self._write_executable(
            "ksail",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            arguments=" $* "
            secret_file='k8s/bases/bootstrap/secret.enc.yaml'
            selector='["stringData"]["ghcr_dockerconfigjson"]'
            [[ "$arguments" == *" workload cipher decrypt $secret_file "* ]]
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
            test -n "$namespace"
            touch "$KUBECTL_CALLED"

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
        path = self.bin_dir / name
        path.write_text(textwrap.dedent(body).lstrip(), encoding="utf-8")
        path.chmod(0o755)

    def _run_helper(
        self,
        config: object,
        helper_args: tuple[str, ...] = (),
        **environment_overrides: str,
    ) -> subprocess.CompletedProcess[str]:
        self.decrypted_config.write_text(json.dumps(config), encoding="utf-8")
        for marker in (
            self.patch_capture,
            self.variables_patch_capture,
            self.kubectl_called,
            self.output_path_log,
            self.registry_read_log,
            self.fanout_log,
        ):
            marker.unlink(missing_ok=True)
        for marker in self.sync_state_dir.iterdir():
            marker.unlink()
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": f"{self.bin_dir}:{environment['PATH']}",
                "FAKE_DECRYPTED_CONFIG": str(self.decrypted_config),
                "PATCH_CAPTURE": str(self.patch_capture),
                "VARIABLES_PATCH_CAPTURE": str(self.variables_patch_capture),
                "KUBECTL_CALLED": str(self.kubectl_called),
                "KSAIL_OUTPUT_PATH_LOG": str(self.output_path_log),
                "REGISTRY_READ_LOG": str(self.registry_read_log),
                "FANOUT_LOG": str(self.fanout_log),
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

    @staticmethod
    def _valid_config() -> dict[str, object]:
        return {
            "auths": {
                "ghcr.io": {
                    "username": "devantler",
                    "password": "fixture-secret-token",
                }
            }
        }

    def test_refreshes_root_and_fanout_without_leaking_plaintext(self) -> None:
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
                "devantler-tech/ksail:latest",
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

    def test_accepts_standard_auth_only_docker_config(self) -> None:
        auth = base64.b64encode(b"devantler:fixture-secret-token").decode()
        config = {"auths": {"ghcr.io": {"auth": auth}}}

        result = self._run_helper(config)

        self.assertEqual(result.returncode, 0, result.stderr)
        patch = json.loads(self.patch_capture.read_text(encoding="utf-8"))
        encoded = patch["data"][".dockerconfigjson"]
        self.assertEqual(json.loads(base64.b64decode(encoded)), config)

    def test_accepts_matching_explicit_and_encoded_auth(self) -> None:
        config = self._valid_config()
        registry_auth = config["auths"]["ghcr.io"]
        registry_auth["auth"] = base64.b64encode(
            b"devantler:fixture-secret-token"
        ).decode()

        result = self._run_helper(config)

        self.assertEqual(result.returncode, 0, result.stderr)

    def test_check_only_preflights_without_patching(self) -> None:
        result = self._run_helper(self._valid_config(), ("--check-only",))

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.kubectl_called.exists())
        self.assertFalse(self.patch_capture.exists())

    def test_missing_or_malformed_registry_auth_fails_closed(self) -> None:
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
        result = self._run_helper(
            self._valid_config(),
            FAKE_CURL_DENY_REPOSITORY="devantler-tech/platform/manifests",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.kubectl_called.exists())

    def test_token_success_without_registry_read_access_prevents_cluster_patch(
        self,
    ) -> None:
        result = self._run_helper(
            self._valid_config(),
            FAKE_CURL_DENY_REPOSITORY="devantler-tech/wedding-app",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.kubectl_called.exists())

    def test_cluster_patch_failure_is_not_hidden(self) -> None:
        result = self._run_helper(self._valid_config(), FAKE_KUBECTL_FAIL="true")

        self.assertEqual(result.returncode, 43)
        self.assertTrue(self.kubectl_called.exists())

    def test_fresh_cluster_without_variables_base_skips_existing_fanout(self) -> None:
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
        result = self._run_helper(
            self._valid_config(),
            FAKE_VARIABLES_BASE_ABSENT="true",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.patch_capture.exists())

    def test_partial_bootstrap_repairs_root_without_forcing_missing_fanout(
        self,
    ) -> None:
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

    def test_partial_fanout_fails_closed_without_bootstrap_mode(self) -> None:
        result = self._run_helper(
            self._valid_config(),
            FAKE_MISSING_FANOUT_RESOURCE="externalsecret/kyverno/ghcr-auth",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.patch_capture.exists())
        self.assertFalse(self.fanout_log.exists())

    def test_partial_bootstrap_without_eso_crds_repairs_root(self) -> None:
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
        result = self._run_helper(
            self._valid_config(),
            FAKE_SYNC_SAME_REFRESH_TIME="true",
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.patch_capture.exists())

    def test_materialised_consumer_mismatch_is_not_hidden(self) -> None:
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

    def test_consumer_staging_precedes_publish_and_is_reasserted_after_update(
        self,
    ) -> None:
        action = ACTION.read_text(encoding="utf-8")

        first_refresh = action.index("id: stage_flux_ghcr_auth")
        push = action.index("run: ksail --config ksail.prod.yaml workload push")
        post_push_refresh = action.index("id: verify_flux_ghcr_auth_after_push")
        reconcile = action.index("id: reconcile")
        cluster_update = action.index(
            "run: ksail --config ksail.prod.yaml cluster update"
        )
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
        workflow = DR_REBUILD.read_text(encoding="utf-8")

        preflight = workflow.index(
            "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only"
        )
        cluster_create = workflow.index(
            "run: ksail --config ksail.prod.yaml cluster create"
        )
        stage = workflow.index("id: stage_flux_ghcr_auth")
        push = workflow.index("run: ksail --config ksail.prod.yaml workload push")
        verify = workflow.index("id: verify_flux_ghcr_auth_after_push")
        fanout_verify = workflow.index("id: verify_flux_ghcr_fanout")
        openbao_restore = workflow.index(
            "name: 🔐 Restore OpenBao from the R2 snapshot mirror"
        )
        post_restore_verify = workflow.index(
            "id: reassert_flux_ghcr_after_restore"
        )
        reconcile = workflow.index(
            "run: ksail --config ksail.prod.yaml workload reconcile"
        )

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
            "if: ${{ inputs.restore }}",
            workflow[post_restore_verify:],
        )
        self.assertEqual(workflow.count("scripts/refresh-flux-ghcr-auth.sh"), 5)


class RequiredPackageCoverageTests(unittest.TestCase):
    """Keep pinned private provider references in the live GHCR preflight."""

    def test_provider_upjet_unifi_reference_is_preflighted(self) -> None:
        manifest = PROVIDER_UPJET_UNIFI.read_text(encoding="utf-8")
        helper = HELPER.read_text(encoding="utf-8")

        package_line = next(
            line.strip()
            for line in manifest.splitlines()
            if line.strip().startswith("package: ghcr.io/")
        )
        package_reference = package_line.removeprefix("package: ghcr.io/")

        self.assertIn(f'"{package_reference}"', helper)


if __name__ == "__main__":
    unittest.main()
