"""Tests for the merge-group production-heal workflow contract validator."""

from __future__ import annotations

import importlib.util
import io
import typing
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import ModuleType


MODULE_PATH = Path(__file__).parents[1] / "validate-merge-group-heal.py"


def load_validator() -> tuple[ModuleType, str, str]:
    """Load the validator while capturing any import-time output."""
    spec = importlib.util.spec_from_file_location("validate_merge_group_heal", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"could not load {MODULE_PATH}")

    module = importlib.util.module_from_spec(spec)
    stdout = io.StringIO()
    stderr = io.StringIO()
    with redirect_stdout(stdout), redirect_stderr(stderr):
        spec.loader.exec_module(module)
    return module, stdout.getvalue(), stderr.getvalue()


class EntrypointTests(unittest.TestCase):
    """Keep imports inert while preserving executable validation."""

    def test_import_is_side_effect_free(self) -> None:
        _, stdout, stderr = load_validator()

        self.assertEqual("", stdout)
        self.assertEqual("", stderr)

    def test_main_executes_validation(self) -> None:
        module, _, _ = load_validator()
        stdout = io.StringIO()

        with redirect_stdout(stdout):
            module.main()

        self.assertEqual("Merge-group heal workflow contract passed.\n", stdout.getvalue())

    def test_fail_is_annotated_as_no_return(self) -> None:
        module, _, _ = load_validator()

        self.assertIs(typing.NoReturn, typing.get_type_hints(module.fail)["return"])


if __name__ == "__main__":
    unittest.main()
