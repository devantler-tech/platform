#!/usr/bin/env python3
"""Render-level regression tests for the default-off Cilium BBR component."""

import os
import pathlib
import subprocess
import tempfile
import textwrap
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
COMPONENT = (
    ROOT
    / "k8s/providers/hetzner/infrastructure/controllers/cilium/components"
    / "bandwidth-manager-bbr"
)
PROD_CONTROLLERS = ROOT / "k8s/providers/hetzner/infrastructure/controllers"


def render(path, unrestricted=False):
    """Render a Kustomize root with kubectl and return its YAML stream."""
    command = ["kubectl", "kustomize", str(path)]
    if unrestricted:
        command.extend(["--load-restrictor", "LoadRestrictionsNone"])
    result = subprocess.run(
        command,
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise AssertionError(result.stderr.strip())
    return result.stdout


def cilium_release(rendered):
    """Return the Cilium HelmRelease document from a rendered YAML stream."""
    for document in rendered.split("\n---\n"):
        if "kind: HelmRelease" in document and "name: cilium" in document:
            return document
    raise AssertionError("render did not contain the Cilium HelmRelease")


class CiliumBandwidthManagerComponentTests(unittest.TestCase):
    """Pin both the default-off and explicit opt-in render states."""

    def test_committed_prod_overlay_keeps_bandwidth_manager_off(self):
        parent = (PROD_CONTROLLERS / "kustomization.yaml").read_text(encoding="utf-8")
        self.assertIn(
            "# - cilium/components/bandwidth-manager-bbr/",
            parent,
        )
        self.assertNotIn(
            "\n  - cilium/components/bandwidth-manager-bbr/",
            parent,
        )
        self.assertNotIn("bandwidthManager:", cilium_release(render(PROD_CONTROLLERS)))

    def test_opt_in_render_enables_bbr_and_preserves_wireguard(self):
        self.assertTrue(COMPONENT.is_dir(), f"missing component: {COMPONENT}")
        with tempfile.TemporaryDirectory(prefix=".platform-cilium-bbr-", dir=ROOT) as tmp:
            base = os.path.relpath(PROD_CONTROLLERS, tmp)
            component = os.path.relpath(COMPONENT, tmp)
            pathlib.Path(tmp, "kustomization.yaml").write_text(
                textwrap.dedent(
                    f"""\
                    ---
                    apiVersion: kustomize.config.k8s.io/v1beta1
                    kind: Kustomization
                    resources:
                      - {base}
                    components:
                      - {component}
                    """
                ),
                encoding="utf-8",
            )
            release = cilium_release(render(tmp, unrestricted=True))

        self.assertIn(
            "bandwidthManager:\n"
            "      bbr: true\n"
            "      bbrHostNamespaceOnly: true\n"
            "      enabled: true",
            release,
        )
        self.assertNotIn("bpf:\n      masquerade: true", release)
        self.assertIn("encryption:\n      enabled: true\n      nodeEncryption: false", release)
        self.assertIn("type: wireguard", release)


if __name__ == "__main__":
    unittest.main()
