"""Regression tests for the EKS smoke role's inline IAM policy."""

import json
import textwrap
import unittest
from pathlib import Path


ROLE_MANIFEST = (
    Path(__file__).resolve().parents[2]
    / "k8s/providers/hetzner/apps/aws/role-eks-ci.yaml"
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


if __name__ == "__main__":
    unittest.main()
