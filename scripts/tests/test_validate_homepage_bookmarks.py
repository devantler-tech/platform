#!/usr/bin/env python3
"""Unit tests for scripts/validate-homepage-bookmarks.py.

Pure stdlib (unittest); no cluster, no network. Run from anywhere:

    python3 -m unittest scripts/tests/test_validate_homepage_bookmarks.py -v

The module under test is loaded by file path (its filename is hyphenated,
matching this repo's script-naming convention, so it cannot be imported with
a normal `import` statement).
"""
import importlib.util
import os
import sys
import textwrap
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MODULE_PATH = os.path.join(ROOT, "scripts", "validate-homepage-bookmarks.py")

spec = importlib.util.spec_from_file_location("validate_homepage_bookmarks", MODULE_PATH)
validator = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = validator
spec.loader.exec_module(validator)


def dedent_block(text):
    """Strip the leading newline/indentation convenience helper for fixtures."""
    return textwrap.dedent(text).strip("\n")


class ParseBookmarksTests(unittest.TestCase):
    def test_parses_single_group_single_entry(self):
        text = dedent_block("""
            - Personal:
                - Personal Site:
                    - icon: github-light
                      href: https://devantler.tech
        """)
        entries = validator.parse_bookmarks(text)
        self.assertEqual(entries, [
            {"group": "Personal", "name": "Personal Site",
             "icon": "github-light", "href": "https://devantler.tech"},
        ])

    def test_parses_multiple_groups_and_entries_in_order(self):
        text = dedent_block("""
            - Developer Tools:
                - Codecov:
                    - icon: si-codecov-#F01F7A
                      href: https://app.codecov.io
                - CodeRabbit:
                    - icon: si-coderabbit-#FF570A
                      href: https://app.coderabbit.ai
            - Kubernetes:
                - ArtifactHUB:
                    - icon: si-artifacthub-#417598
                      href: https://artifacthub.io
        """)
        entries = validator.parse_bookmarks(text)
        self.assertEqual(len(entries), 3)
        self.assertEqual(
            [(e["group"], e["name"]) for e in entries],
            [
                ("Developer Tools", "Codecov"),
                ("Developer Tools", "CodeRabbit"),
                ("Kubernetes", "ArtifactHUB"),
            ],
        )
        coderabbit = entries[1]
        self.assertEqual(coderabbit["icon"], "si-coderabbit-#FF570A")
        self.assertEqual(coderabbit["href"], "https://app.coderabbit.ai")

    def test_entry_missing_icon_line_has_none_icon(self):
        text = dedent_block("""
            - Developer Tools:
                - Broken:
                    - href: https://example.com
        """)
        entries = validator.parse_bookmarks(text)
        self.assertEqual(len(entries), 1)
        self.assertIsNone(entries[0]["icon"])
        self.assertEqual(entries[0]["href"], "https://example.com")

    def test_entry_missing_href_line_is_not_emitted(self):
        # An entry is only recorded once its href line is seen (mirrors the
        # ConfigMap's fixed icon-then-href ordering); an icon with no href
        # never produces an entry.
        text = dedent_block("""
            - Developer Tools:
                - Broken:
                    - icon: mdi-alert
        """)
        entries = validator.parse_bookmarks(text)
        self.assertEqual(entries, [])

    def test_empty_text_yields_no_entries(self):
        self.assertEqual(validator.parse_bookmarks(""), [])

    def test_blank_lines_between_entries_are_ignored(self):
        text = dedent_block("""
            - Developer Tools:
                - Codecov:
                    - icon: si-codecov-#F01F7A
                      href: https://app.codecov.io

                - CodeRabbit:
                    - icon: si-coderabbit-#FF570A
                      href: https://app.coderabbit.ai
        """)
        entries = validator.parse_bookmarks(text)
        self.assertEqual(len(entries), 2)
        self.assertEqual(entries[1]["name"], "CodeRabbit")


class ValidateEntriesTests(unittest.TestCase):
    def _entry(self, group="Developer Tools", name="CodeRabbit",
               icon="si-coderabbit-#FF570A", href="https://app.coderabbit.ai"):
        return {"group": group, "name": name, "icon": icon, "href": href}

    def test_valid_entry_with_colored_simple_icon_has_no_problems(self):
        problems = validator.validate_entries([self._entry()])
        self.assertEqual(problems, [])

    def test_valid_entry_with_bare_slug_icon_has_no_problems(self):
        problems = validator.validate_entries([self._entry(icon="docker")])
        self.assertEqual(problems, [])

    def test_valid_entry_with_mdi_icon_has_no_problems(self):
        problems = validator.validate_entries([self._entry(icon="mdi-bank-outline")])
        self.assertEqual(problems, [])

    def test_missing_icon_is_reported(self):
        problems = validator.validate_entries([self._entry(icon=None)])
        self.assertEqual(len(problems), 1)
        self.assertIn("missing icon", problems[0])

    def test_missing_href_is_reported(self):
        problems = validator.validate_entries([self._entry(href=None)])
        self.assertEqual(len(problems), 1)
        self.assertIn("missing href", problems[0])

    def test_http_href_is_rejected(self):
        problems = validator.validate_entries([self._entry(href="http://app.coderabbit.ai")])
        self.assertEqual(len(problems), 1)
        self.assertIn("must be an https:// URL", problems[0])

    def test_href_without_scheme_is_rejected(self):
        problems = validator.validate_entries([self._entry(href="app.coderabbit.ai")])
        self.assertEqual(len(problems), 1)
        self.assertIn("must be an https:// URL", problems[0])

    def test_icon_with_invalid_characters_is_rejected(self):
        problems = validator.validate_entries([self._entry(icon="si coderabbit")])
        self.assertEqual(len(problems), 1)
        self.assertIn("does not match", problems[0])

    def test_icon_with_lowercase_hex_color_is_accepted(self):
        # e.g. Renovate's `si-renovatebot-#007fa0` in the real ConfigMap.
        problems = validator.validate_entries([self._entry(icon="si-renovatebot-#007fa0")])
        self.assertEqual(problems, [])

    def test_duplicate_names_within_same_group_are_reported(self):
        entries = [self._entry(), self._entry()]
        problems = validator.validate_entries(entries)
        self.assertTrue(any("duplicate bookmark name" in p for p in problems))

    def test_same_name_in_different_groups_is_not_a_duplicate(self):
        entries = [
            self._entry(group="Developer Tools", name="GitHub"),
            self._entry(group="GitHub", name="GitHub"),
        ]
        problems = validator.validate_entries(entries)
        self.assertEqual(problems, [])

    def test_empty_entry_list_has_no_problems(self):
        self.assertEqual(validator.validate_entries([]), [])


class ExtractBookmarksYamlTests(unittest.TestCase):
    def _write(self, tmp_path, content):
        with open(tmp_path, "w", encoding="utf-8") as handle:
            handle.write(content)

    def test_extracts_only_the_bookmarks_block(self):
        import tempfile
        content = dedent_block("""
            data:
              services.yaml: |
                - Network:
                    - Cloudflare:
                        icon: cloudflare
                        href: https://dash.cloudflare.com
              bookmarks.yaml: |
                - Developer Tools:
                    - CodeRabbit:
                        - icon: si-coderabbit-#FF570A
                          href: https://app.coderabbit.ai
              docker.yaml: ""
        """) + "\n"
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as handle:
            handle.write(content)
            path = handle.name
        try:
            block = validator.extract_bookmarks_yaml(path)
        finally:
            os.remove(path)
        self.assertIn("CodeRabbit", block)
        self.assertNotIn("Cloudflare", block)
        self.assertNotIn("docker.yaml", block)

    def test_missing_key_raises_value_error(self):
        import tempfile
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as handle:
            handle.write("data:\n  services.yaml: |\n    - Network: []\n")
            path = handle.name
        try:
            with self.assertRaises(ValueError):
                validator.extract_bookmarks_yaml(path)
        finally:
            os.remove(path)


class RealConfigMapRegressionTests(unittest.TestCase):
    """Guard the actual PR change against the real ConfigMap on disk."""

    @classmethod
    def setUpClass(cls):
        cls.entries = validator.parse_bookmarks(
            validator.extract_bookmarks_yaml(validator.CONFIG_MAP)
        )

    def test_coderabbit_bookmark_was_added_under_developer_tools(self):
        matches = [
            e for e in self.entries
            if e["group"] == "Developer Tools" and e["name"] == "CodeRabbit"
        ]
        self.assertEqual(len(matches), 1, "expected exactly one CodeRabbit bookmark")
        self.assertEqual(matches[0]["icon"], "si-coderabbit-#FF570A")
        self.assertEqual(matches[0]["href"], "https://app.coderabbit.ai")

    def test_coderabbit_entry_is_positioned_after_codecov(self):
        names = [e["name"] for e in self.entries if e["group"] == "Developer Tools"]
        self.assertIn("Codecov", names)
        self.assertIn("CodeRabbit", names)
        self.assertLess(names.index("Codecov"), names.index("CodeRabbit"))

    def test_real_config_map_has_no_validation_problems(self):
        problems = validator.validate_entries(self.entries)
        self.assertEqual(problems, [], f"unexpected problems: {problems}")


if __name__ == "__main__":
    unittest.main()