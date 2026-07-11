#!/usr/bin/env python3
"""Unit tests for scripts/generate-kubescape-exceptions.py.

Requires PyYAML (like the module under test); no cluster, no network. Run
from anywhere:

    python3 -m unittest scripts/tests/test_generate_kubescape_exceptions.py -v

The module under test is loaded by file path (its filename is hyphenated,
matching this repo's script-naming convention, so it cannot be imported with
a normal `import` statement).
"""

import importlib.util
import os
import tempfile
import textwrap
import unittest

MODULE_PATH = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "generate-kubescape-exceptions.py",
)
spec = importlib.util.spec_from_file_location(
    "generate_kubescape_exceptions", MODULE_PATH
)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def cse(name, posture, match=None, reason="documented reason"):
    """Build a minimal ClusterSecurityException document for tests."""
    doc = {
        "apiVersion": "kubescape.io/v1beta1",
        "kind": "ClusterSecurityException",
        "metadata": {"name": name},
        "spec": {"reason": reason, "posture": posture},
    }
    if match is not None:
        doc["spec"]["match"] = match
    return doc


IGNORE = [{"controlID": "C-0002", "action": "ignore"}]


class ConvertDocumentTests(unittest.TestCase):
    """Behaviour of convert_document for each supported/rejected CR shape."""

    def test_non_cse_documents_are_skipped(self):
        """Non-CSE documents (and empty docs) convert to None, not errors."""
        self.assertIsNone(
            mod.convert_document({"kind": "Kustomization", "resources": []}, "f")
        )
        self.assertIsNone(mod.convert_document(None, "f"))

    def test_cluster_wide_exception_gets_match_all_namespace(self):
        """A CSE without match applies cluster-wide via a `.*` namespace."""
        policy = mod.convert_document(cse("all", IGNORE), "f")
        self.assertEqual(
            policy["resources"],
            [{"designatorType": "Attributes", "attributes": {"namespace": ".*"}}],
        )

    def test_policy_shape_and_control_anchoring(self):
        """The generated policy carries the exact Kubescape policy shape."""
        policy = mod.convert_document(cse("shape", IGNORE), "f")
        self.assertEqual(policy["name"], "shape")
        self.assertEqual(policy["policyType"], "postureExceptionPolicy")
        self.assertEqual(policy["actions"], ["alertOnly"])
        self.assertEqual(policy["reason"], "documented reason")
        (control,) = policy["posturePolicies"]
        # Anchored exact-match regex so C-0002 cannot substring-match C-0020.
        self.assertTrue(control["controlID"].startswith("^C"))
        self.assertTrue(control["controlID"].endswith("0002$"))

    def test_reason_block_scalar_is_flattened_to_one_line(self):
        """Multi-line reason scalars are flattened to one line."""
        policy = mod.convert_document(
            cse("flat", IGNORE, reason="line one\nline two"), "f"
        )
        self.assertEqual(policy["reason"], "line one line two")

    def test_resources_map_to_attribute_designators(self):
        """match.resources entries become anchored Attributes designators."""
        match = {
            "resources": [
                {
                    "apiGroup": "rbac.authorization.k8s.io",
                    "kind": "ClusterRoleBinding",
                    "name": "^velero-server$",
                },
                {"apiGroup": "batch", "kind": "CronJob"},
            ]
        }
        policy = mod.convert_document(cse("res", IGNORE, match), "f")
        self.assertEqual(
            policy["resources"],
            [
                {
                    "designatorType": "Attributes",
                    "attributes": {
                        "kind": "^ClusterRoleBinding$",
                        "name": "^velero-server$",
                    },
                },
                {"designatorType": "Attributes", "attributes": {"kind": "^CronJob$"}},
            ],
        )

    def test_namespace_selector_maps_to_alternation(self):
        """A metadata.name In [...] selector becomes a namespace alternation."""
        match = {
            "namespaceSelector": {
                "matchExpressions": [
                    {
                        "key": "kubernetes.io/metadata.name",
                        "operator": "In",
                        "values": ["kube-system", "velero"],
                    }
                ]
            }
        }
        policy = mod.convert_document(cse("ns", IGNORE, match), "f")
        self.assertEqual(
            policy["resources"],
            [
                {
                    "designatorType": "Attributes",
                    "attributes": {"namespace": "^(kube\\-system|velero)$"},
                }
            ],
        )

    def test_unknown_match_key_fails_closed(self):
        """An unsupported match key aborts instead of being dropped."""
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, {"labelSelector": {}}), "f")

    def test_match_labels_selector_fails_closed(self):
        """matchLabels namespace selectors are unsupported and abort."""
        match = {"namespaceSelector": {"matchLabels": {"team": "x"}}}
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, match), "f")

    def test_non_ignore_action_fails_closed(self):
        """A posture action other than `ignore` aborts."""
        posture = [{"controlID": "C-0002", "action": "alert"}]
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", posture), "f")

    def test_resource_without_kind_fails_closed(self):
        """A match.resources entry without a kind aborts."""
        match = {"resources": [{"name": "^x$"}]}
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, match), "f")

    def test_both_match_shapes_fails_closed(self):
        """Setting both match.resources and match.namespaceSelector aborts."""
        match = {
            "resources": [{"kind": "Job"}],
            "namespaceSelector": {"matchExpressions": []},
        }
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, match), "f")

    def test_empty_resources_fails_closed(self):
        """An explicit but empty match.resources aborts instead of widening
        the exception to cluster-wide scope."""
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, {"resources": []}), "f")

    def test_empty_namespace_selector_fails_closed(self):
        """An explicit but empty match.namespaceSelector aborts instead of
        widening the exception to cluster-wide scope."""
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, {"namespaceSelector": {}}), "f")

    def test_partially_anchored_value_fails_closed(self):
        """A value anchored on only one end is ambiguous and aborts."""
        for value in ("^half-anchored", "half-anchored$"):
            match = {"resources": [{"kind": "Job", "name": value}]}
            with self.assertRaises(SystemExit):
                mod.convert_document(cse("bad", IGNORE, match), "f")

    def test_fully_anchored_value_passes_through_unescaped(self):
        """A fully `^...$`-anchored value is kept as the author's regex."""
        match = {"resources": [{"kind": "Job", "name": "^(a|b)-server$"}]}
        policy = mod.convert_document(cse("ok", IGNORE, match), "f")
        self.assertEqual(
            policy["resources"][0]["attributes"]["name"], "^(a|b)-server$"
        )


class GenerateTests(unittest.TestCase):
    """Behaviour of generate() over a directory of CR files."""

    def write(self, directory, filename, text):
        """Write a dedented YAML fixture file into the test directory."""
        with open(os.path.join(directory, filename), "w", encoding="utf-8") as f:
            f.write(textwrap.dedent(text))

    def test_generates_sorted_policies_and_skips_kustomization(self):
        """Policies come back name-sorted; kustomization.yaml is skipped."""
        with tempfile.TemporaryDirectory() as directory:
            self.write(
                directory,
                "kustomization.yaml",
                """\
                apiVersion: kustomize.config.k8s.io/v1beta1
                kind: Kustomization
                resources:
                  - b.yaml
                """,
            )
            self.write(
                directory,
                "b.yaml",
                """\
                apiVersion: kubescape.io/v1beta1
                kind: ClusterSecurityException
                metadata:
                  name: zeta
                spec:
                  reason: >-
                    multi-line
                    reason text
                  posture:
                    - controlID: C-0057
                      action: ignore
                """,
            )
            self.write(
                directory,
                "a.yaml",
                """\
                apiVersion: kubescape.io/v1beta1
                kind: ClusterSecurityException
                metadata:
                  name: alpha
                spec:
                  posture:
                    - controlID: C-0002
                      action: ignore
                  match:
                    resources:
                      - apiGroup: rbac.authorization.k8s.io
                        kind: ClusterRoleBinding
                        name: ^kubevirt-operator$
                """,
            )
            policies = mod.generate(directory)
        self.assertEqual([p["name"] for p in policies], ["alpha", "zeta"])
        self.assertEqual(policies[1]["reason"], "multi-line reason text")

    def test_duplicate_names_fail_closed(self):
        """Two CSEs with the same name abort the run."""
        with tempfile.TemporaryDirectory() as directory:
            for filename in ("a.yaml", "b.yaml"):
                self.write(
                    directory,
                    filename,
                    """\
                    apiVersion: kubescape.io/v1beta1
                    kind: ClusterSecurityException
                    metadata:
                      name: same
                    spec:
                      posture:
                        - controlID: C-0002
                          action: ignore
                    """,
                )
            with self.assertRaises(SystemExit):
                mod.generate(directory)

    def test_empty_directory_fails_closed(self):
        """A directory without any CSE documents aborts the run."""
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(SystemExit):
                mod.generate(directory)

    def test_real_repo_crs_convert_cleanly(self):
        """The live CSE directory must always convert — the CI invariant."""
        policies = mod.generate(mod.DEFAULT_DIR)
        names = {p["name"] for p in policies}
        self.assertIn("exec-into-container-rbac", names)
        for policy in policies:
            self.assertTrue(policy["resources"])
            self.assertTrue(policy["posturePolicies"])


if __name__ == "__main__":
    unittest.main()
