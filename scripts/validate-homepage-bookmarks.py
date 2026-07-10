#!/usr/bin/env python3
"""Validate the Homepage dashboard's bookmarks.yaml (see AGENTS.md).

k8s/bases/apps/homepage/config-map.yaml embeds the Homepage app's
`bookmarks.yaml` as a YAML block scalar. Manifest schema validation treats
that blob as an opaque string, so a bookmark missing its `icon`/`href`, using
a non-https link, or duplicated under the same group only surfaces at
runtime as a broken card on the dashboard. This gate parses every bookmark
entry and checks its shape so a malformed entry fails the PR instead.

Pure stdlib; no cluster, no network. Run from anywhere:

    python3 scripts/validate-homepage-bookmarks.py

Checks, per bookmark entry:
  1. An `icon` is present and is either a bare icon slug (e.g. `docker`,
     `github-light`) or a Simple Icons / MDI slug (`si-<name>` or
     `mdi-<name>`), optionally suffixed with a `-#RRGGBB` color override.
  2. An `href` is present and is an `https://` URL.
  3. No two bookmarks in the same group share the same name.
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
K8S = os.path.join(ROOT, "k8s")
CONFIG_MAP = os.path.join(K8S, "bases", "apps", "homepage", "config-map.yaml")

BOOKMARKS_KEY_RE = re.compile(r"^(\s*)bookmarks\.yaml:\s*\|\s*$")
GROUP_RE = re.compile(r"^-\s+(.+):\s*$")
ITEM_RE = re.compile(r"^-\s+(.+):\s*$")
ICON_RE = re.compile(r"^-\s*icon:\s*(.+?)\s*$")
HREF_RE = re.compile(r"^(?:-\s*)?href:\s*(.+?)\s*$")

ICON_PATTERN = re.compile(r"^[A-Za-z0-9]+(-[A-Za-z0-9]+)*(-#[0-9A-Fa-f]{6})?$")
HREF_PATTERN = re.compile(r"^https://\S+$")


def extract_bookmarks_yaml(path):
    """Return the dedented text of the ConfigMap's `data.bookmarks.yaml` block."""
    with open(path, encoding="utf-8") as handle:
        lines = handle.read().splitlines()
    for index, line in enumerate(lines):
        match = BOOKMARKS_KEY_RE.match(line)
        if not match:
            continue
        key_indent = len(match.group(1))
        block = []
        cursor = index + 1
        while cursor < len(lines):
            candidate = lines[cursor]
            if candidate.strip() and len(candidate) - len(candidate.lstrip()) <= key_indent:
                break
            block.append(candidate)
            cursor += 1
        indents = [len(l) - len(l.lstrip()) for l in block if l.strip()]
        if not indents:
            return ""
        dedent = min(indents)
        return "\n".join(l[dedent:] if l.strip() else "" for l in block)
    raise ValueError(f"'bookmarks.yaml: |' key not found in {path}")


def parse_bookmarks(text):
    """Parse dedented bookmarks.yaml text into a flat list of entry dicts.

    Each entry is {"group": str, "name": str, "icon": str|None, "href": str|None},
    relying on the fixed 4/8/10-space indentation the block always uses:
    group (0), bookmark name (4), icon (8), href (10).
    """
    entries = []
    current_group = None
    current_name = None
    current_icon = None
    for line in text.splitlines():
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip(" "))
        stripped = line.strip()
        if indent == 0:
            match = GROUP_RE.match(stripped)
            if match:
                current_group = match.group(1)
                current_name = None
                current_icon = None
        elif indent == 4:
            match = ITEM_RE.match(stripped)
            if match:
                current_name = match.group(1)
                current_icon = None
        elif indent == 8:
            match = ICON_RE.match(stripped)
            if match:
                current_icon = match.group(1)
                continue
            match = HREF_RE.match(stripped)
            if match and current_group and current_name:
                entries.append({
                    "group": current_group,
                    "name": current_name,
                    "icon": current_icon,
                    "href": match.group(1),
                })
        elif indent == 10:
            match = HREF_RE.match(stripped)
            if match and current_group and current_name:
                entries.append({
                    "group": current_group,
                    "name": current_name,
                    "icon": current_icon,
                    "href": match.group(1),
                })
    return entries


def validate_entries(entries):
    """Return a list of human-readable problem strings for the given entries."""
    problems = []
    seen = set()
    for entry in entries:
        where = f"{entry['group']} -> {entry['name']}"
        key = (entry["group"], entry["name"])
        if key in seen:
            problems.append(f"{where}: duplicate bookmark name in this group")
        seen.add(key)
        icon = entry["icon"]
        if not icon:
            problems.append(f"{where}: missing icon")
        elif not ICON_PATTERN.match(icon):
            problems.append(f"{where}: icon '{icon}' does not match <slug>[-#RRGGBB]")
        href = entry["href"]
        if not href:
            problems.append(f"{where}: missing href")
        elif not HREF_PATTERN.match(href):
            problems.append(f"{where}: href '{href}' must be an https:// URL")
    return problems


def main():
    entries = parse_bookmarks(extract_bookmarks_yaml(CONFIG_MAP))
    problems = validate_entries(entries)
    if not entries:
        problems.append("No bookmark entries were parsed — parser may be out of "
                         "sync with config-map.yaml's bookmarks.yaml block")

    if problems:
        print(f"\n✗ Homepage bookmarks violation(s) ({len(problems)}):")
        for problem in sorted(problems):
            print("   " + problem)
        print(f"\n{len(problems)} homepage bookmarks violation(s). "
              "See scripts/validate-homepage-bookmarks.py.")
        sys.exit(1)
    print(f"✓ {len(entries)} homepage bookmark(s) valid.")


if __name__ == "__main__":
    main()