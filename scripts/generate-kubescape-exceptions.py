#!/usr/bin/env python3
"""Generate a Kubescape exceptions file from the ClusterSecurityException CRs.

The platform documents every justified posture finding as a
`ClusterSecurityException` CR in
`k8s/bases/infrastructure/cluster-security-exceptions/` — that directory is the
single source of truth for what is excepted and why. The in-cluster
kubescape-operator consumes the CRs directly, but the offline CI scan
(`ksail workload scan --exceptions <file>`) takes Kubescape's native format: a
JSON array of PostureExceptionPolicy objects. This script derives that file
from the CRs at scan time, so CI and the cluster can never disagree about the
exception set.

Fail-closed by design: any CR shape this converter does not recognise (an
unknown `spec.match` key, a posture action other than `ignore`, a
namespaceSelector that isn't the `kubernetes.io/metadata.name In [...]`
expression) aborts with a non-zero exit instead of silently dropping or
widening an exception.

Requires PyYAML (preinstalled on GitHub-hosted runners; on macOS the system
`/usr/bin/python3` ships it). Run from anywhere:

    python3 scripts/generate-kubescape-exceptions.py -o /tmp/exceptions.json
"""

import argparse
import json
import os
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit(
        "PyYAML is required (preinstalled on GitHub runners; locally try "
        "/usr/bin/python3 or `python3 -m pip install pyyaml`)."
    )

DEFAULT_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "k8s",
    "bases",
    "infrastructure",
    "cluster-security-exceptions",
)
NAMESPACE_NAME_KEY = "kubernetes.io/metadata.name"


def anchor(value):
    """Anchor a plain value into an exact-match regex; keep explicit regexes.

    CR authors write resource `name` fields as anchored regexes already
    (`^velero-server$`) but plain `kind`/controlID values; Kubescape treats
    every designator attribute and controlID as a regex, so an unanchored
    plain value would substring-match (C-0002 would also match C-0020).
    """
    if value.startswith("^") or value.endswith("$"):
        return value
    return f"^{re.escape(value)}$"


def fail(path, name, message):
    sys.exit(f"{path}: ClusterSecurityException {name!r}: {message}")


def convert_namespace_selector(selector, path, name):
    """Map a namespaceSelector to a single namespace-regex designator."""
    unknown = set(selector) - {"matchExpressions"}
    if unknown:
        fail(path, name, f"unsupported namespaceSelector keys {sorted(unknown)}")
    expressions = selector.get("matchExpressions") or []
    if len(expressions) != 1:
        fail(path, name, "expected exactly one namespaceSelector matchExpression")
    expr = expressions[0]
    if expr.get("key") != NAMESPACE_NAME_KEY or expr.get("operator") != "In":
        fail(
            path,
            name,
            f"only `{NAMESPACE_NAME_KEY} In [...]` matchExpressions are supported",
        )
    values = expr.get("values") or []
    if not values:
        fail(path, name, "namespaceSelector matchExpression has no values")
    pattern = "^(" + "|".join(re.escape(v) for v in values) + ")$"
    return [{"designatorType": "Attributes", "attributes": {"namespace": pattern}}]


def convert_resources(resources, path, name):
    """Map `match.resources` entries to Attributes designators.

    `apiGroup` is intentionally dropped: PostureExceptionPolicy designator
    attributes have no apiGroup field, and the anchored kind+name pair is
    what scopes the exception (same mapping the in-cluster operator applies).
    """
    designators = []
    for entry in resources:
        unknown = set(entry) - {"apiGroup", "kind", "name", "namespace"}
        if unknown:
            fail(path, name, f"unsupported match.resources keys {sorted(unknown)}")
        if "kind" not in entry:
            fail(path, name, "match.resources entry without a kind")
        attributes = {"kind": anchor(entry["kind"])}
        if "name" in entry:
            attributes["name"] = anchor(entry["name"])
        if "namespace" in entry:
            attributes["namespace"] = anchor(entry["namespace"])
        designators.append(
            {"designatorType": "Attributes", "attributes": attributes}
        )
    return designators


def convert_document(doc, path):
    """Convert one ClusterSecurityException document; None for other kinds."""
    if not isinstance(doc, dict) or doc.get("kind") != "ClusterSecurityException":
        return None
    name = (doc.get("metadata") or {}).get("name")
    if not name:
        fail(path, "<unnamed>", "missing metadata.name")
    spec = doc.get("spec") or {}

    posture = spec.get("posture") or []
    if not posture:
        fail(path, name, "spec.posture is empty")
    policies = []
    for control in posture:
        action = control.get("action")
        if action != "ignore":
            fail(path, name, f"unsupported posture action {action!r}")
        control_id = control.get("controlID")
        if not control_id:
            fail(path, name, "posture entry without a controlID")
        policies.append({"controlID": anchor(control_id)})

    match = spec.get("match") or {}
    unknown = set(match) - {"resources", "namespaceSelector"}
    if unknown:
        fail(path, name, f"unsupported match keys {sorted(unknown)}")
    if "resources" in match and "namespaceSelector" in match:
        fail(path, name, "both match.resources and match.namespaceSelector set")
    if match.get("resources"):
        resources = convert_resources(match["resources"], path, name)
    elif match.get("namespaceSelector"):
        resources = convert_namespace_selector(
            match["namespaceSelector"], path, name
        )
    else:
        # No match => the exception applies cluster-wide for its controls.
        resources = [
            {"designatorType": "Attributes", "attributes": {"namespace": ".*"}}
        ]

    policy = {
        "name": name,
        "policyType": "postureExceptionPolicy",
        "actions": ["alertOnly"],
        "resources": resources,
        "posturePolicies": policies,
    }
    if spec.get("reason"):
        policy["reason"] = " ".join(str(spec["reason"]).split())
    return policy


def generate(directory):
    policies = []
    seen = set()
    for filename in sorted(os.listdir(directory)):
        if not filename.endswith((".yaml", ".yml")):
            continue
        path = os.path.join(directory, filename)
        with open(path, encoding="utf-8") as handle:
            documents = list(yaml.safe_load_all(handle))
        for doc in documents:
            policy = convert_document(doc, path)
            if policy is None:
                continue
            if policy["name"] in seen:
                fail(path, policy["name"], "duplicate exception name")
            seen.add(policy["name"])
            policies.append(policy)
    if not policies:
        sys.exit(f"{directory}: no ClusterSecurityException documents found")
    return sorted(policies, key=lambda p: p["name"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "directory",
        nargs="?",
        default=DEFAULT_DIR,
        help="directory holding the ClusterSecurityException CRs",
    )
    parser.add_argument(
        "-o",
        "--output",
        help="output file (stdout if omitted)",
    )
    args = parser.parse_args()

    policies = generate(args.directory)
    rendered = json.dumps(policies, indent=2) + "\n"
    if args.output:
        with open(args.output, "w", encoding="utf-8") as handle:
            handle.write(rendered)
        print(
            f"wrote {len(policies)} exception policies to {args.output}",
            file=sys.stderr,
        )
    else:
        sys.stdout.write(rendered)


if __name__ == "__main__":
    main()
