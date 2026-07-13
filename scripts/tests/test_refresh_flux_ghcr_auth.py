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
        self.kubectl_called = self.workspace / "kubectl-called"
        self.output_path_log = self.workspace / "ksail-output-path"
        self.curl_scope_log = self.workspace / "curl-scopes"

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
            "docker",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            test "$1" = buildx
            test "$2" = imagetools
            test "$3" = inspect
            test "$4" = ghcr.io/devantler-tech/platform/manifests:latest
            test -f "$DOCKER_CONFIG/config.json"
            if [[ "${FAKE_DOCKER_FAIL:-false}" == true ]]; then
              echo 'registry pull preflight denied' >&2
              exit 42
            fi
            """,
        )
        self._write_executable(
            "curl",
            """
            #!/usr/bin/env bash
            set -euo pipefail
            config=""
            scope=""
            while (($#)); do
              case "$1" in
                --config)
                  config="$2"
                  shift 2
                  ;;
                --data-urlencode)
                  case "$2" in scope=*) scope="${2#scope=}" ;; esac
                  shift 2
                  ;;
                *) shift ;;
              esac
            done
            test -f "$config"
            test -n "$scope"
            printf '%s\n' "$scope" >> "$CURL_SCOPE_LOG"
            if [[ "$scope" == *"${FAKE_CURL_DENY_REPOSITORY:-disabled}"* ]]; then
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
            [[ "$arguments" == *" --namespace flux-system "* ]]
            [[ "$arguments" == *" patch secret ksail-registry-credentials "* ]]
            [[ "$arguments" == *" --type=merge "* ]]
            patch_file=""
            for argument in "$@"; do
              case "$argument" in
                --patch-file=*) patch_file="${argument#*=}" ;;
              esac
            done
            test -n "$patch_file"
            touch "$KUBECTL_CALLED"
            cp "$patch_file" "$PATCH_CAPTURE"
            if [[ "${FAKE_KUBECTL_FAIL:-false}" == true ]]; then
              echo 'cluster patch failed' >&2
              exit 43
            fi
            echo 'secret/ksail-registry-credentials patched'
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
            self.kubectl_called,
            self.output_path_log,
            self.curl_scope_log,
        ):
            marker.unlink(missing_ok=True)
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": f"{self.bin_dir}:{environment['PATH']}",
                "FAKE_DECRYPTED_CONFIG": str(self.decrypted_config),
                "PATCH_CAPTURE": str(self.patch_capture),
                "KUBECTL_CALLED": str(self.kubectl_called),
                "KSAIL_OUTPUT_PATH_LOG": str(self.output_path_log),
                "CURL_SCOPE_LOG": str(self.curl_scope_log),
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

    def test_refreshes_only_the_root_secret_and_cleans_plaintext(self) -> None:
        config = self._valid_config()

        result = self._run_helper(config)

        self.assertEqual(result.returncode, 0, result.stderr)
        patch = json.loads(self.patch_capture.read_text(encoding="utf-8"))
        encoded = patch["data"][".dockerconfigjson"]

        self.assertEqual(json.loads(base64.b64decode(encoded)), config)
        temporary_config = Path(self.output_path_log.read_text(encoding="utf-8"))
        self.assertFalse(temporary_config.exists())
        self.assertNotIn("fixture-secret-token", result.stdout + result.stderr)
        self.assertEqual(
            self.curl_scope_log.read_text(encoding="utf-8").splitlines(),
            [
                "repository:devantler-tech/platform/manifests:pull",
                "repository:devantler-tech/wedding-app/manifests:pull",
                "repository:devantler-tech/ascoachingogvaner/manifests:pull",
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
            {"auths": {}},
            "not-a-docker-config",
        ]

        for config in invalid_configs:
            with self.subTest(config=config):
                result = self._run_helper(config)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.kubectl_called.exists())

    def test_registry_denial_prevents_cluster_patch(self) -> None:
        result = self._run_helper(self._valid_config(), FAKE_DOCKER_FAIL="true")

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.kubectl_called.exists())

    def test_tenant_package_denial_prevents_cluster_patch(self) -> None:
        result = self._run_helper(
            self._valid_config(),
            FAKE_CURL_DENY_REPOSITORY="devantler-tech/wedding-app/manifests",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.kubectl_called.exists())

    def test_cluster_patch_failure_is_not_hidden(self) -> None:
        result = self._run_helper(self._valid_config(), FAKE_KUBECTL_FAIL="true")

        self.assertEqual(result.returncode, 43)
        self.assertTrue(self.kubectl_called.exists())


class DeployActionOrderingTests(unittest.TestCase):
    """Keep credential refreshes on both sides of KSail-owned mutations."""

    def test_refresh_precedes_reconcile_and_is_reasserted_after_update(self) -> None:
        action = ACTION.read_text(encoding="utf-8")

        first_refresh = action.index("id: preflight_flux_ghcr_auth")
        push = action.index("run: ksail --config ksail.prod.yaml workload push")
        post_push_refresh = action.index("id: reassert_flux_ghcr_auth_after_push")
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
            "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only",
            action,
        )
        self.assertIn(
            "if: always() && steps.preflight_flux_ghcr_auth.outcome == 'success'",
            action,
        )
        self.assertEqual(action.count("scripts/refresh-flux-ghcr-auth.sh"), 3)

    def test_disaster_rebuild_refreshes_root_auth_before_reconcile(self) -> None:
        workflow = DR_REBUILD.read_text(encoding="utf-8")

        refresh = workflow.index("scripts/refresh-flux-ghcr-auth.sh")
        reconcile = workflow.index(
            "run: ksail --config ksail.prod.yaml workload reconcile"
        )

        self.assertLess(refresh, reconcile)


if __name__ == "__main__":
    unittest.main()
