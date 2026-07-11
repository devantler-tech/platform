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
import sys
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
    def test_non_cse_documents_are_skipped(self):
        self.assertIsNone(
            mod.convert_document({"kind": "Kustomization", "resources": []}, "f")
        )
        self.assertIsNone(mod.convert_document(None, "f"))

    def test_cluster_wide_exception_gets_match_all_namespace(self):
        policy = mod.convert_document(cse("all", IGNORE), "f")
        self.assertEqual(
            policy["resources"],
            [{"designatorType": "Attributes", "attributes": {"namespace": ".*"}}],
        )

    def test_policy_shape_and_control_anchoring(self):
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
        policy = mod.convert_document(
            cse("flat", IGNORE, reason="line one\nline two"), "f"
        )
        self.assertEqual(policy["reason"], "line one line two")

    def test_resources_map_to_attribute_designators(self):
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
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, {"labelSelector": {}}), "f")

    def test_match_labels_selector_fails_closed(self):
        match = {"namespaceSelector": {"matchLabels": {"team": "x"}}}
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, match), "f")

    def test_non_ignore_action_fails_closed(self):
        posture = [{"controlID": "C-0002", "action": "alert"}]
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", posture), "f")

    def test_resource_without_kind_fails_closed(self):
        match = {"resources": [{"name": "^x$"}]}
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, match), "f")

    def test_both_match_shapes_fails_closed(self):
        match = {
            "resources": [{"kind": "Job"}],
            "namespaceSelector": {"matchExpressions": []},
        }
        with self.assertRaises(SystemExit):
            mod.convert_document(cse("bad", IGNORE, match), "f")


class GenerateTests(unittest.TestCase):
    def write(self, directory, filename, text):
        with open(os.path.join(directory, filename), "w", encoding="utf-8") as f:
            f.write(textwrap.dedent(text))

    def test_generates_sorted_policies_and_skips_kustomization(self):
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
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(SystemExit):
                mod.generate(directory)

    def test_real_repo_crs_convert_cleanly(self):
        # The live CSE directory must always convert — this is the same
        # invariant the CI scan step relies on every run.
        policies = mod.generate(mod.DEFAULT_DIR)
        names = {p["name"] for p in policies}
        self.assertIn("exec-into-container-rbac", names)
        for policy in policies:
            self.assertTrue(policy["resources"])
            self.assertTrue(policy["posturePolicies"])


if __name__ == "__main__":
    unittest.main()
