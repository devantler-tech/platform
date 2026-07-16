"""Behavioral tests for reconciliation-Alert namespace coverage."""

from __future__ import annotations

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[2]
VALIDATOR = ROOT / "scripts" / "validate-alert-coverage.sh"
CI_WORKFLOW = ROOT / ".github" / "workflows" / "ci.yaml"
LAYERS = (
    "k8s/clusters/prod",
    "k8s/providers/hetzner/bootstrap",
    "k8s/providers/hetzner/infrastructure/controllers",
    "k8s/providers/hetzner/infrastructure",
    "k8s/providers/hetzner/apps",
)


class ValidateAlertCoverageTests(unittest.TestCase):
    """Exercise the validator against isolated Kustomize fixtures."""

    def setUp(self) -> None:
        """Create a minimal repository-shaped fixture for every test."""
        self.temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp_dir.cleanup)
        self.workspace = Path(self.temp_dir.name)
        script = self.workspace / "scripts" / VALIDATOR.name
        script.parent.mkdir(parents=True)
        shutil.copy2(VALIDATOR, script)
        self.script = script

        for index, layer in enumerate(LAYERS):
            self._write_layer(layer, index)
        self._write_alert(wildcard=True)

    def _write_layer(self, relative_path: str, index: int) -> None:
        """Write one valid layer containing both watched Flux kinds."""
        layer = self.workspace / relative_path
        layer.mkdir(parents=True, exist_ok=True)
        (layer / "kustomization.yaml").write_text(
            textwrap.dedent(
                """\
                apiVersion: kustomize.config.k8s.io/v1beta1
                kind: Kustomization
                resources:
                  - resources.yaml
                """
            ),
            encoding="utf-8",
        )
        (layer / "resources.yaml").write_text(
            textwrap.dedent(
                f"""\
                apiVersion: helm.toolkit.fluxcd.io/v2
                kind: HelmRelease
                metadata:
                  name: release-{index}
                  namespace: flux-system
                spec: {{}}
                ---
                apiVersion: kustomize.toolkit.fluxcd.io/v1
                kind: Kustomization
                metadata:
                  name: layer-{index}
                  namespace: flux-system
                spec: {{}}
                """
            ),
            encoding="utf-8",
        )

    def _write_alert(self, *, wildcard: bool) -> None:
        """Write an Alert with wildcard or resource-specific event sources."""
        alert = (
            self.workspace
            / "k8s"
            / "providers"
            / "hetzner"
            / "infrastructure"
            / "flux-notifications"
            / "alert.yaml"
        )
        alert.parent.mkdir(parents=True, exist_ok=True)
        name = "*" if wildcard else "one-resource"
        alert.write_text(
            textwrap.dedent(
                f"""\
                apiVersion: notification.toolkit.fluxcd.io/v1beta3
                kind: Alert
                metadata:
                  name: reconciliation
                  namespace: flux-system
                spec:
                  eventSources:
                    - kind: HelmRelease
                      name: "{name}"
                      namespace: flux-system
                    - kind: Kustomization
                      name: "{name}"
                      namespace: flux-system
                """
            ),
            encoding="utf-8",
        )

    def _run_validator(
        self, **environment_overrides: str
    ) -> subprocess.CompletedProcess[str]:
        """Run the copied validator with the real local kubectl and yq."""
        environment = os.environ.copy()
        environment.update(environment_overrides)
        return subprocess.run(
            ["bash", str(self.script)],
            cwd=self.workspace,
            env=environment,
            check=False,
            capture_output=True,
            text=True,
        )

    def test_valid_wildcard_alert_covers_every_rendered_namespace(self) -> None:
        """Keep the known-good whole-namespace coverage path green."""
        result = self._run_validator()

        self.assertEqual(result.returncode, 0, result.stderr)

    def test_missing_layer_fails_closed_instead_of_being_skipped(self) -> None:
        """A missing Kustomization must invalidate the coverage proof."""
        missing = self.workspace / LAYERS[-1] / "kustomization.yaml"
        missing.unlink()

        result = self._run_validator()

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(LAYERS[-1], result.stdout + result.stderr)

    def test_named_event_source_does_not_cover_its_whole_namespace(self) -> None:
        """Only name '*' proves coverage for every resource in a namespace."""
        self._write_alert(wildcard=False)

        result = self._run_validator()

        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("does not watch every namespace", result.stdout + result.stderr)

    def test_watched_resource_without_namespace_fails_closed(self) -> None:
        """Reject malformed namespaced Flux resources instead of dropping them."""
        resource_file = self.workspace / LAYERS[0] / "resources.yaml"
        for kind, name in (
            ("HelmRelease", "release-0"),
            ("Kustomization", "layer-0"),
        ):
            with self.subTest(kind=kind):
                self._write_layer(LAYERS[0], 0)
                resources = resource_file.read_text(encoding="utf-8")
                namespaced = (
                    f"kind: {kind}\nmetadata:\n"
                    f"  name: {name}\n  namespace: flux-system"
                )
                self.assertIn(namespaced, resources)
                resource_file.write_text(
                    resources.replace(
                        namespaced,
                        f"kind: {kind}\nmetadata:\n  name: {name}",
                        1,
                    ),
                    encoding="utf-8",
                )

                result = self._run_validator()

                output = result.stdout + result.stderr
                self.assertNotEqual(result.returncode, 0, output)
                self.assertIn("missing metadata.namespace", output)
                self.assertIn(kind, output)
                self.assertIn(name, output)

    def test_yq_diagnostics_remain_visible_on_query_failure(self) -> None:
        """Keep the parser's own reason when a fail-closed query errors."""
        bin_dir = self.workspace / "bin"
        bin_dir.mkdir()
        yq = bin_dir / "yq"
        yq.write_text(
            "#!/usr/bin/env bash\n"
            "echo fixture-yq-query-diagnostic >&2\n"
            "exit 72\n",
            encoding="utf-8",
        )
        yq.chmod(0o755)

        result = self._run_validator(
            PATH=f"{bin_dir}:{os.environ['PATH']}",
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("fixture-yq-query-diagnostic", result.stderr)

    def test_ci_runs_the_behavioral_regressions_when_they_change(self) -> None:
        """Keep the validator's failure-mode coverage in the required CI job."""
        workflow = CI_WORKFLOW.read_text(encoding="utf-8")

        self.assertIn("'scripts/tests/test_validate_alert_coverage.py'", workflow)
        self.assertIn(
            "python3 -m unittest scripts.tests.test_validate_alert_coverage",
            workflow,
        )


if __name__ == "__main__":
    unittest.main()
