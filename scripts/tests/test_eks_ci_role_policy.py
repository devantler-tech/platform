"""Regression tests for the EKS smoke role's inline IAM policy."""

import copy
import hashlib
import json
import textwrap
import unittest
from pathlib import Path


ROLE_MANIFEST = (
    Path(__file__).resolve().parents[2]
    / "k8s/providers/hetzner/apps/aws/role-eks-ci.yaml"
)
CI_WORKFLOW = Path(__file__).resolve().parents[2] / ".github/workflows/ci.yaml"
EXPECTED_STATEMENT_SIDS = {
    "CloudFormationRead",
    "CloudFormationScoped",
    "Ec2AndAutoscaling",
    "EksRead",
    "EksScoped",
    "IamCreateRoleBounded",
    "IamOidcProviderLifecycle",
    "IamReadCallerIdentity",
    "IamReadForEksctl",
    "IamScopedInstanceProfiles",
    "IamScopedPolicyLifecycle",
    "IamScopedRoleLifecycle",
    "IamServiceLinkedRoles",
    "SsmAmiLookup",
    "StsIdentity",
}
EXPECTED_EKS_SCOPED_RESOURCES = {
    "arn:aws:eks:*:939001610192:access-entry/st-eks-*/*/*/*/*",
    "arn:aws:eks:*:939001610192:accessEntry/st-eks-*/*/*/*/*",
    "arn:aws:eks:*:939001610192:addon/st-eks-*/*/*",
    "arn:aws:eks:*:939001610192:cluster/st-eks-*",
    "arn:aws:eks:*:939001610192:nodegroup/st-eks-*/*/*",
}
# Canonical JSON keeps the entire approved Allow policy fail-closed, including
# actions, resources, conditions, statement count, and policy version. The
# readable Sid/resource assertions below make the security-critical diff clear.
EXPECTED_POLICY_SHA256 = (
    "60e3086a6d3dac0092ffe8264c04ebae783c0d38f19a3cf073ed8991085a4df8"
)


def load_inline_policy() -> dict:
    """Extract the JSON block scalar without adding a YAML dependency."""
    lines = ROLE_MANIFEST.read_text(encoding="utf-8").splitlines()
    marker = "        policy: |"
    start = lines.index(marker) + 1
    policy_lines: list[str] = []

    for line in lines[start:]:
        indentation = len(line) - len(line.lstrip())
        if line and indentation <= 8:
            break
        policy_lines.append(line)

    return json.loads(textwrap.dedent("\n".join(policy_lines)))


def assert_inline_policy_shape(policy: dict) -> None:
    """Reject any unreviewed grant or mutation in the approved Allow policy."""
    statements = policy.get("Statement")
    if not isinstance(statements, list):
        raise AssertionError("policy Statement must be a list")

    sids = [statement.get("Sid") for statement in statements]
    if len(sids) != len(EXPECTED_STATEMENT_SIDS) or set(sids) != EXPECTED_STATEMENT_SIDS:
        raise AssertionError(f"unexpected statement set: {sids}")

    by_sid = {statement["Sid"]: statement for statement in statements}
    scoped_resources = by_sid["EksScoped"].get("Resource")
    if not isinstance(scoped_resources, list):
        raise AssertionError("EksScoped resources must be a list")
    if set(scoped_resources) != EXPECTED_EKS_SCOPED_RESOURCES:
        raise AssertionError(f"unexpected EksScoped resources: {scoped_resources}")

    canonical = json.dumps(policy, sort_keys=True, separators=(",", ":")).encode()
    fingerprint = hashlib.sha256(canonical).hexdigest()
    if fingerprint != EXPECTED_POLICY_SHA256:
        raise AssertionError(f"unapproved policy shape: sha256={fingerprint}")


class TestEKSCIRolePolicy(unittest.TestCase):
    """Lock the read-only exception and the surrounding security boundaries."""

    def test_cluster_version_discovery_is_read_only(self) -> None:
        statements = {item["Sid"]: item for item in load_inline_policy()["Statement"]}

        self.assertEqual(
            {
                "eks:DescribeAddonConfiguration",
                "eks:DescribeAddonVersions",
                "eks:DescribeClusterVersions",
                "eks:ListClusters",
            },
            set(statements["EksRead"]["Action"]),
        )
        self.assertEqual("*", statements["EksRead"]["Resource"])
        self.assertNotEqual("*", statements["EksScoped"]["Resource"])

        create_role = statements["IamCreateRoleBounded"]
        boundary = create_role["Condition"]["StringEquals"]["iam:PermissionsBoundary"]
        self.assertTrue(boundary.endswith(":policy/eks-ci-smoke-boundary"))

    def test_complete_policy_shape_is_pinned(self) -> None:
        assert_inline_policy_shape(load_inline_policy())

    def test_unexpected_allow_statement_is_rejected(self) -> None:
        policy = copy.deepcopy(load_inline_policy())
        policy["Statement"].append(
            {
                "Sid": "UnexpectedGrant",
                "Effect": "Allow",
                "Action": "iam:*",
                "Resource": "*",
            }
        )

        with self.assertRaises(AssertionError):
            assert_inline_policy_shape(policy)

    def test_scoped_resource_wildcard_member_is_rejected(self) -> None:
        policy = copy.deepcopy(load_inline_policy())
        statements = {item["Sid"]: item for item in policy["Statement"]}
        statements["EksScoped"]["Resource"].append("*")

        with self.assertRaises(AssertionError):
            assert_inline_policy_shape(policy)

    def test_policy_test_triggers_manifest_validation(self) -> None:
        workflow = CI_WORKFLOW.read_text(encoding="utf-8")
        k8s_filter = workflow.split("            k8s:\n", maxsplit=1)[1].split(
            "            bridge_validation:", maxsplit=1
        )[0]
        self.assertIn("- 'scripts/tests/test_eks_ci_role_policy.py'", k8s_filter)


if __name__ == "__main__":
    unittest.main()
