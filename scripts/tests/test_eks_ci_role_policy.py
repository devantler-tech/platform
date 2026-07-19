"""Regression tests for the EKS smoke role's inline IAM policy."""

import copy
import hashlib
import json
import tempfile
import textwrap
import unittest
from pathlib import Path


ROLE_MANIFEST = (
    Path(__file__).resolve().parents[2]
    / "k8s/providers/hetzner/apps/aws/role-eks-ci.yaml"
)
BOUNDARY_MANIFEST = (
    Path(__file__).resolve().parents[2]
    / "k8s/providers/hetzner/apps/aws/policy-eks-ci-smoke-boundary.yaml"
)
CI_WORKFLOW = Path(__file__).resolve().parents[2] / ".github/workflows/ci.yaml"
EXPECTED_ROLE_FOR_PROVIDER_KEYS = [
    "description",
    "maxSessionDuration",
    "assumeRolePolicy",
    "inlinePolicy",
]
EXPECTED_BOUNDARY_FOR_PROVIDER_KEYS = ["description", "policy"]
EXPECTED_TRUST_POLICY_SHA256 = (
    "85d5d45343f9eac5fdc35717c85c88c5b0f8fde9eddffb169c3a223617fd0a5e"
)
EXPECTED_BOUNDARY_POLICY_SHA256 = (
    "e617004bce71a65f92934c4f7575d7559a290afe7a17363ce12db8ad7b519610"
)
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


def extract_json_blocks(lines: list[str], marker: str) -> list[dict]:
    """Extract every JSON block scalar matching an exact YAML indentation."""
    marker_indent = len(marker) - len(marker.lstrip())
    documents: list[dict] = []

    for marker_index, line in enumerate(lines):
        if line != marker:
            continue

        document_lines: list[str] = []
        for document_line in lines[marker_index + 1 :]:
            indentation = len(document_line) - len(document_line.lstrip())
            if document_line and indentation <= marker_indent:
                break
            document_lines.append(document_line)
        documents.append(json.loads(textwrap.dedent("\n".join(document_lines))))

    return documents


def for_provider_keys(lines: list[str]) -> list[str]:
    """Return all direct spec.forProvider keys, including unexpected grants."""
    start = lines.index("  forProvider:") + 1
    keys: list[str] = []

    for line in lines[start:]:
        indentation = len(line) - len(line.lstrip())
        if line and indentation <= 2:
            break
        if line and indentation == 4:
            keys.append(line.strip().split(":", maxsplit=1)[0])

    return keys


def load_role_authorization(manifest: Path = ROLE_MANIFEST) -> dict:
    """Parse the role's complete authorization surface using the stdlib."""
    lines = manifest.read_text(encoding="utf-8").splitlines()
    names = [
        line.removeprefix("      - name: ")
        for line in lines
        if line.startswith("      - name: ")
    ]
    policies = extract_json_blocks(lines, "        policy: |")
    inline_policies = [
        {
            "name": names[index] if index < len(names) else None,
            "policy": policy,
        }
        for index, policy in enumerate(policies)
    ]
    if len(names) > len(policies):
        inline_policies.extend(
            {"name": name, "policy": None} for name in names[len(policies) :]
        )

    max_session_lines = [
        line for line in lines if line.startswith("    maxSessionDuration: ")
    ]

    return {
        "for_provider_keys": for_provider_keys(lines),
        "max_session_duration": max_session_lines,
        "trust_policies": extract_json_blocks(lines, "    assumeRolePolicy: |"),
        "inline_policies": inline_policies,
    }


def load_boundary_authorization() -> dict:
    """Parse the complete permissions-boundary authorization surface."""
    lines = BOUNDARY_MANIFEST.read_text(encoding="utf-8").splitlines()
    policies = extract_json_blocks(lines, "    policy: |")

    return {
        "for_provider_keys": for_provider_keys(lines),
        "policy_count": len(policies),
        "policy": policies[0] if policies else None,
    }


def load_inline_policy() -> dict:
    """Return the single approved inline policy after parsing the collection."""
    authorization = load_role_authorization()
    policies = authorization["inline_policies"]
    if len(policies) != 1 or policies[0]["policy"] is None:
        raise AssertionError("expected exactly one parseable inline policy")

    return policies[0]["policy"]


def canonical_sha256(document: dict) -> str:
    """Hash a JSON document independent of object-key formatting."""
    canonical = json.dumps(document, sort_keys=True, separators=(",", ":")).encode()

    return hashlib.sha256(canonical).hexdigest()


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

    fingerprint = canonical_sha256(policy)
    if fingerprint != EXPECTED_POLICY_SHA256:
        raise AssertionError(f"unapproved policy shape: sha256={fingerprint}")


def assert_role_authorization_shape(authorization: dict) -> None:
    """Pin trust, attachments, session duration, and all inline policies."""
    if authorization["for_provider_keys"] != EXPECTED_ROLE_FOR_PROVIDER_KEYS:
        raise AssertionError(
            f"unexpected Role forProvider keys: {authorization['for_provider_keys']}"
        )
    if authorization["max_session_duration"] != ["    maxSessionDuration: 7200"]:
        raise AssertionError("unexpected Role maxSessionDuration")

    trust_policies = authorization["trust_policies"]
    if len(trust_policies) != 1:
        raise AssertionError("expected exactly one assumeRolePolicy")
    trust_fingerprint = canonical_sha256(trust_policies[0])
    if trust_fingerprint != EXPECTED_TRUST_POLICY_SHA256:
        raise AssertionError(f"unapproved trust policy: sha256={trust_fingerprint}")

    inline_policies = authorization["inline_policies"]
    if len(inline_policies) != 1 or inline_policies[0]["name"] != "eks-ci-smoke":
        raise AssertionError(f"unexpected inlinePolicy collection: {inline_policies}")
    assert_inline_policy_shape(inline_policies[0]["policy"])


def assert_boundary_authorization_shape(authorization: dict) -> None:
    """Pin the complete policy that caps roles minted by the smoke identity."""
    if authorization["for_provider_keys"] != EXPECTED_BOUNDARY_FOR_PROVIDER_KEYS:
        raise AssertionError(
            "unexpected boundary forProvider keys: "
            f"{authorization['for_provider_keys']}"
        )
    if authorization["policy_count"] != 1 or authorization["policy"] is None:
        raise AssertionError("expected exactly one permissions-boundary policy")

    fingerprint = canonical_sha256(authorization["policy"])
    if fingerprint != EXPECTED_BOUNDARY_POLICY_SHA256:
        raise AssertionError(f"unapproved boundary policy: sha256={fingerprint}")


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

    def test_complete_role_authorization_surface_is_pinned(self) -> None:
        assert_role_authorization_shape(load_role_authorization())

    def test_additional_inline_policy_is_rejected(self) -> None:
        authorization = copy.deepcopy(load_role_authorization())
        authorization["inline_policies"].append(
            {
                "name": "unexpected",
                "policy": {
                    "Version": "2012-10-17",
                    "Statement": [
                        {
                            "Effect": "Allow",
                            "Action": "iam:*",
                            "Resource": "*",
                        }
                    ],
                },
            }
        )

        with self.assertRaises(AssertionError):
            assert_role_authorization_shape(authorization)

    def test_loader_sees_additional_inline_policy_in_manifest(self) -> None:
        source = ROLE_MANIFEST.read_text(encoding="utf-8")
        additional_policy = """
      - name: unexpected
        policy: |
          {"Version":"2012-10-17","Statement":[]}
"""
        mutated = source.replace(
            "\n  providerConfigRef:", f"{additional_policy}\n  providerConfigRef:", 1
        )
        self.assertNotEqual(source, mutated)

        with tempfile.TemporaryDirectory() as temp_dir:
            manifest = Path(temp_dir) / "role.yaml"
            manifest.write_text(mutated, encoding="utf-8")

            with self.assertRaises(AssertionError):
                assert_role_authorization_shape(load_role_authorization(manifest))

    def test_managed_policy_attachment_is_rejected(self) -> None:
        authorization = copy.deepcopy(load_role_authorization())
        authorization["for_provider_keys"].append("managedPolicyArns")

        with self.assertRaises(AssertionError):
            assert_role_authorization_shape(authorization)

    def test_boundary_policy_shape_is_pinned(self) -> None:
        assert_boundary_authorization_shape(load_boundary_authorization())

    def test_boundary_grant_mutation_is_rejected(self) -> None:
        authorization = copy.deepcopy(load_boundary_authorization())
        authorization["policy"]["Statement"][0]["Action"].append("iam:*")

        with self.assertRaises(AssertionError):
            assert_boundary_authorization_shape(authorization)

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
