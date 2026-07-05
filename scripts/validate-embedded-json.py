#!/usr/bin/env python3
"""Validate embedded JSON blobs in ConfigMaps (see AGENTS.md).

Some ConfigMaps carry whole JSON documents as YAML block scalars — e.g. the
Headlamp Kubescape exceptions ConfigMap's `data.exceptionPolicies`. Manifest
schema validation treats such a blob as an opaque string, so a stray comma or
missing bracket ships silently and only fails at consumption time — where an
empty exceptions view reads identical to a clean posture. This gate
json-parses every registered embedded-JSON ConfigMap key so a syntax error
fails the PR instead.

Pure stdlib; no cluster, no network. Run from anywhere:

    python3 scripts/validate-embedded-json.py

A ConfigMap `data` key is checked when it is listed in REGISTERED_KEYS or ends
in `.json`. SOPS-encrypted files (*.enc.yaml) and ENC[...] values are skipped.
Registered keys must use a literal block scalar (`|`) or a single-line value —
a folded scalar (`>`) rewraps lines and would corrupt the JSON.
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
K8S = os.path.join(ROOT, "k8s")

# ConfigMap data keys known to embed a JSON document. Extend when a new
# embedded-JSON key is introduced (keys ending in .json are checked implicitly).
REGISTERED_KEYS = {"exceptionPolicies"}

KEY_LINE = re.compile(r"^(\s*)([^\s:#][^:]*):[ \t]*(.*)$")


def is_registered(key):
    return key in REGISTERED_KEYS or key.endswith(".json")


def rel(path):
    return os.path.relpath(path, ROOT)


def block_scalar(lines, start, key_indent):
    """Collect a literal block scalar's dedented text and its end line index.

    `start` is the index of the first line after the `key: |` line. The block
    is every following line blank or indented deeper than the key; content is
    dedented by the minimal indent of its non-blank lines (YAML's detected
    block indentation).
    """
    block = []
    index = start
    while index < len(lines):
        line = lines[index]
        if line.strip() and len(line) - len(line.lstrip()) <= key_indent:
            break
        block.append(line)
        index += 1
    indents = [len(l3) - len(l3.lstrip()) for l3 in block if l3.strip()]
    if not indents:
        return "", index
    dedent = min(indents)
    return "\n".join(l3[dedent:] if l3.strip() else "" for l3 in block), index


def embedded_json_values(path):
    """Yield (key, value, line_number, style) for registered keys in ConfigMap
    data sections of a manifest file. style is 'block', 'folded' or 'plain'."""
    with open(path, encoding="utf-8", errors="replace") as handle:
        text = handle.read()
    offset = 1  # file line (1-based) of the current chunk's first line
    for chunk in re.split(r"(?m)^---[ \t]*$", text):
        lines = chunk.splitlines()
        first_line = offset
        # A chunk after a separator starts with the separator line's tail as a
        # leading "" line, so the chunk's lines map 1:1 onto file lines.
        offset += len(lines)
        if not re.search(r"(?m)^kind:[ \t]*ConfigMap[ \t]*$", chunk):
            continue
        in_data = False
        data_key_indent = None
        index = 0
        while index < len(lines):
            line = lines[index]
            match = KEY_LINE.match(line)
            index += 1
            if not match:
                continue
            indent = len(match.group(1))
            key, value = match.group(2), match.group(3)
            if indent == 0:
                in_data = key == "data" and not value
                data_key_indent = None
                continue
            if not in_data:
                continue
            if data_key_indent is None:
                data_key_indent = indent
            if indent != data_key_indent or not is_registered(key):
                continue
            line_no = first_line + index - 1  # index already points past the key line
            if value.startswith("|"):
                content, index = block_scalar(lines, index, indent)
                yield key, content, line_no, "block"
            elif value.startswith(">"):
                content, index = block_scalar(lines, index, indent)
                yield key, content, line_no, "folded"
            elif value:
                if len(value) > 1 and value[0] == value[-1] and value[0] in "'\"":
                    value = value[1:-1]
                yield key, value, line_no, "plain"


def main():
    invalid, folded = [], []
    checked = 0
    for dirpath, _, filenames in os.walk(K8S):
        for filename in filenames:
            if not filename.endswith((".yaml", ".yml")) or filename.endswith(".enc.yaml"):
                continue
            path = os.path.join(dirpath, filename)
            for key, value, line_no, style in embedded_json_values(path):
                if "ENC[" in value:
                    continue
                where = f"{rel(path)}:{line_no}"
                if style == "folded":
                    folded.append(f"{where}  ({key})")
                    continue
                checked += 1
                try:
                    json.loads(value)
                except json.JSONDecodeError as error:
                    invalid.append(f"{where}  ({key}: {error})")

    problems = len(invalid) + len(folded)
    if invalid:
        print(f"\n✗ Embedded JSON does not parse ({len(invalid)}):")
        for item in sorted(invalid):
            print("   " + item)
    if folded:
        print(f"\n✗ Embedded JSON in a folded scalar — use a literal block scalar '|' ({len(folded)}):")
        for item in sorted(folded):
            print("   " + item)

    if problems:
        print(f"\n{problems} embedded-JSON violation(s). See scripts/validate-embedded-json.py.")
        sys.exit(1)
    print(f"✓ {checked} embedded JSON blob(s) parse cleanly.")


if __name__ == "__main__":
    main()
