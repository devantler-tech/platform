#!/usr/bin/env python3
"""Validate the merge-queue production-heal job's fail-closed condition."""

from __future__ import annotations

import re
import sys
from pathlib import Path
from typing import NoReturn


def fail(message: str) -> NoReturn:
    print(f"merge-group heal contract: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    """Validate the production-heal job against its fail-closed contract."""
    repo_root = Path(__file__).resolve().parents[1]
    workflow_path = repo_root / ".github" / "workflows" / "ci.yaml"
    workflow = workflow_path.read_text(encoding="utf-8")

    job_match = re.search(
        r"^  heal-prod-on-failure:\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)",
        workflow,
        flags=re.MULTILINE | re.DOTALL,
    )
    if job_match is None:
        fail("missing heal-prod-on-failure job")

    job = job_match.group("body")
    for pattern, description in (
        (r"^    needs: \[changes, deploy-prod\]$", "deploy dependencies"),
        (r"^      group: prod-deploy$", "shared production lock"),
        (r"^      cancel-in-progress: false$", "non-preempting production lock"),
        (r"^          ref: main$", "current-main checkout"),
    ):
        if re.search(pattern, job, flags=re.MULTILINE) is None:
            fail(f"heal job is missing {description}")

    condition_match = re.search(
        r"^    if: >-\n(?P<condition>(?:      .*\n)+)",
        job,
        flags=re.MULTILINE,
    )
    if condition_match is None:
        fail("heal job must use an explicit multiline condition")

    condition = condition_match.group("condition")
    normalized_condition = " ".join(
        line.strip() for line in condition.splitlines() if line.strip()
    )
    expected_condition = (
        "always() && "
        "github.event_name == 'merge_group' && "
        "needs.changes.outputs.k8s == 'true' && "
        "(needs.deploy-prod.result == 'failure' || "
        "needs.deploy-prod.result == 'cancelled')"
    )
    if normalized_condition != expected_condition:
        fail(
            "heal condition must cover exactly failed and cancelled deploys "
            "while excluding success"
        )

    print("Merge-group heal workflow contract passed.")


if __name__ == "__main__":
    main()
