#!/usr/bin/env python3
"""Enforce the repo's manifest folder/file naming conventions (see AGENTS.md).

Pure stdlib; no cluster, no network. Run from anywhere:

    python3 scripts/validate-naming.py

Exits non-zero (and prints a grouped report) on any violation. Checks:
  1. Every directory under k8s/ is kebab-case.
  2. Exactly one Kubernetes resource per file (vendored upstream bundles exempt).
  3. Flux Kustomization CRs (kustomize.toolkit.fluxcd.io) live only in
     flux-kustomization*.yaml.
  4. Kustomize build files (kustomize.config.k8s.io) live only in kustomization.yaml.
  5. In a component folder, a single-resource file's name leads with the
     kebab-cased Kind (<kind>.yaml or <kind>-<purpose>.yaml). CR folders, patch
     fragments (under patches/) and kustomization.yaml are exempt.
  6. A folder that groups multiple instances of a single (non-workload) Kind is a
     CR folder and must be named the kebab-cased plural of that Kind
     (e.g. VerticalPodAutoscaler -> vertical-pod-autoscalers/). Organizational
     subfolders inside a known CR folder are exempt.
  7. Patch fragments live under a patches/ directory and never carry a
     redundant -patch suffix (the directory already marks them): a -patch stem
     inside patches/ is redundant, and one outside patches/ is a misplaced
     fragment.
  8. Files under patches/ follow the CR-folder naming convention — an
     intent-describing <verb>-<purpose>.yaml — and must not lead with the
     patched resource's Kind (a Flux Kustomization CR patch keeps the
     flux-kustomization prefix per check 3).
  9. Talos machine-config patches (talos*/ at the repo root) hold ONE YAML
     document per file, in kebab-case, intent-describing <verb>-<purpose>.yaml
     files (no -patch suffix, not led by a document kind). The k8s-specific
     rules (Kind-led filenames, patches/ placement, flux-kustomization prefix)
     do not apply to them.
"""
import os, re, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
K8S = os.path.join(ROOT, "k8s")

# Vendored upstream operator bundles — synced verbatim, exempt from one-per-file.
ONE_RESOURCE_EXEMPT = {
    "k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml",
    "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml",
}

# CR folders: files are named <verb>-<purpose>.yaml (Kind implied by the folder).
# Extend this list when introducing a new plural-Kind folder.
CR_DIR_PATHS = [
    "k8s/bases/infrastructure/cluster-policies",
    "k8s/bases/infrastructure/cluster-role-bindings",
    "k8s/bases/infrastructure/cluster-roles",
    "k8s/bases/infrastructure/cluster-secret-stores",
    "k8s/bases/infrastructure/limit-ranges",
    "k8s/bases/infrastructure/cluster-security-exceptions",
    "k8s/bases/infrastructure/tracing-policies",
    "k8s/bases/infrastructure/external-secrets",
    "k8s/bases/bootstrap/priority-classes",
    "k8s/providers/docker/infrastructure/cluster-issuers",
    "k8s/providers/hetzner/infrastructure/cluster-issuers",
    "k8s/providers/hetzner/infrastructure/cluster-policies",
    "k8s/providers/hetzner/infrastructure/volume-snapshot-classes",
    "k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers",
]

# Kinds that define a component (a folder of them is named by app, not a CR folder).
WORKLOAD_KINDS = {
    "HelmRelease", "HelmRepository", "Deployment", "StatefulSet", "DaemonSet",
    "ReplicaSet", "Pod", "Job", "CronJob", "OCIRepository", "Kustomization", "Component",
}

def pluralize(word):
    """Pluralize the last hyphen-segment of a kebab name (English rules)."""
    head, _, last = word.rpartition("-")
    if last.endswith(("s", "x", "z", "ch", "sh")):
        last += "es"
    elif last.endswith("y") and (len(last) < 2 or last[-2] not in "aeiou"):
        last = last[:-1] + "ies"
    else:
        last += "s"
    return f"{head}-{last}" if head else last

KEBAB = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")

def kebab(kind):
    return re.sub(r"(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])", "-", kind).lower()

def rel(p):
    return os.path.relpath(p, ROOT)

def in_cr(r):
    return any(r == d or r.startswith(d + "/") for d in CR_DIR_PATHS)

def is_patch(r):
    return "/patches/" in r

def docs_with_kind(path):
    """Return [(apiVersion, kind)] for every top-level document declaring a kind."""
    with open(path, encoding="utf-8", errors="replace") as f:
        text = f.read()
    out = []
    for chunk in re.split(r"(?m)^---[ \t]*$", text):
        kind = api = None
        for line in chunk.splitlines():
            if re.match(r"^kind:\s*\S", line):
                kind = line.split(":", 1)[1].strip()
            elif re.match(r"^apiVersion:\s*\S", line):
                api = line.split(":", 1)[1].strip()
        if kind:
            out.append((api or "", kind))
    return out

def count_docs(path):
    """Count YAML documents with any non-comment content (kind-less included)."""
    with open(path, encoding="utf-8", errors="replace") as f:
        text = f.read()
    return sum(
        1 for chunk in re.split(r"(?m)^---[ \t]*$", text)
        if any(l.strip() and not l.lstrip().startswith("#") for l in chunk.splitlines())
    )

def main():
    bad_dirs, multi, flux_bad, build_bad, kind_bad, cr_name_bad = [], [], [], [], [], []
    patch_suffix, patch_misplaced, patch_kind_bad = [], [], []
    folder_kinds = {}  # dirpath -> [kind, ...] for real single-resource files

    for dirpath, dirnames, filenames in os.walk(K8S):
        for dn in dirnames:
            if not KEBAB.match(dn):
                bad_dirs.append(rel(os.path.join(dirpath, dn)))
        for fn in filenames:
            if not fn.endswith((".yaml", ".yml")):
                continue
            r = rel(os.path.join(dirpath, fn))
            stem = fn[:-9] if fn.endswith(".enc.yaml") else fn.rsplit(".", 1)[0]
            if stem.endswith("-patch"):
                (patch_suffix if is_patch(r) else patch_misplaced).append(r)
            docs = docs_with_kind(os.path.join(dirpath, fn))
            if len(docs) == 0:
                continue  # JSON6902 patch fragment / non-resource
            if len(docs) > 1:
                if r not in ONE_RESOURCE_EXEMPT:
                    multi.append((r, [k for _, k in docs]))
                continue
            api, kind = docs[0]
            if fn != "kustomization.yaml" and not is_patch(r):
                folder_kinds.setdefault(dirpath, []).append(kind)
            if kind == "Kustomization" and api.startswith("kustomize.toolkit.fluxcd.io"):
                if not fn.startswith("flux-kustomization"):
                    flux_bad.append(r)
                continue
            if kind in ("Kustomization", "Component") and api.startswith("kustomize.config.k8s.io"):
                if fn != "kustomization.yaml":
                    build_bad.append(r)
                continue
            if fn == "kustomization.yaml" or in_cr(r):
                continue
            kb = kebab(kind)
            if is_patch(r):
                if stem == kb or stem.startswith(kb + "-"):
                    patch_kind_bad.append((r, kind, kb))
                continue
            if not (stem == kb or stem.startswith(kb + "-")):
                kind_bad.append((r, kind, kb))

    # Check 9: Talos machine-config patch dirs (talos*/ at the repo root) —
    # kebab-case names, one YAML document per file, intent naming (no -patch
    # suffix, not led by a document kind).
    talos_multi, talos_kind_bad = [], []
    for talos_dir in sorted(d for d in os.listdir(ROOT)
                            if d.startswith("talos") and os.path.isdir(os.path.join(ROOT, d))):
        for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, talos_dir)):
            for dn in dirnames:
                if not KEBAB.match(dn):
                    bad_dirs.append(rel(os.path.join(dirpath, dn)))
            for fn in filenames:
                if not fn.endswith((".yaml", ".yml")):
                    continue
                path = os.path.join(dirpath, fn)
                r = rel(path)
                stem = fn.rsplit(".", 1)[0]
                if not KEBAB.match(stem):
                    bad_dirs.append(r)
                if stem.endswith("-patch"):
                    patch_suffix.append(r)
                if count_docs(path) > 1:
                    talos_multi.append((r, count_docs(path)))
                    continue
                docs = docs_with_kind(path)
                if len(docs) == 1:
                    kb = kebab(docs[0][1])
                    if stem == kb or stem.startswith(kb + "-"):
                        talos_kind_bad.append((r, docs[0][1], kb))

    # Check 6: a folder grouping >=2 instances of one non-workload Kind is a CR
    # folder and must be named the kebab-cased plural of that Kind.
    for folder, kinds in folder_kinds.items():
        if len(kinds) < 2 or len(set(kinds)) != 1:
            continue
        kind = kinds[0]
        if kind in WORKLOAD_KINDS:
            continue
        rfolder = rel(folder)
        if any(rfolder.startswith(d + "/") for d in CR_DIR_PATHS):
            continue  # organizational subfolder inside a known CR folder
        expected = pluralize(kebab(kind))
        if os.path.basename(rfolder) != expected:
            cr_name_bad.append((rfolder, kind, expected))

    groups = [
        ("Directories or filenames not kebab-case", bad_dirs, lambda x: x),
        ("Files with more than one resource", multi, lambda x: f"{x[0]}  ->  {x[1]}"),
        ("Flux Kustomization CRs not named flux-kustomization*.yaml", flux_bad, lambda x: x),
        ("Kustomize build files not named kustomization.yaml", build_bad, lambda x: x),
        ("Filename does not lead with its Kind", kind_bad,
         lambda x: f"{x[0]}  (kind {x[1]} -> expected {x[2]}.yaml or {x[2]}-<purpose>.yaml)"),
        ("CR-grouping folder not named by Kind plural", cr_name_bad,
         lambda x: f"{x[0]}  ({x[1]} grouping -> expected folder '{x[2]}/')"),
        ("Patch fragments outside a patches/ directory", patch_misplaced,
         lambda x: f"{x}  (move into a patches/ folder and drop the -patch suffix)"),
        ("Patch filenames with redundant -patch suffix", patch_suffix,
         lambda x: f"{x}  (the patches/ folder already marks it; name by intent)"),
        ("Patch filename leads with the patched Kind instead of intent", patch_kind_bad,
         lambda x: f"{x[0]}  (kind {x[1]} -> name it <verb>-<purpose>.yaml, not {x[2]}-*)"),
        ("Talos patch files with more than one YAML document", talos_multi,
         lambda x: f"{x[0]}  ({x[1]} documents -> split, one per file)"),
        ("Talos patch filename leads with the document kind instead of intent", talos_kind_bad,
         lambda x: f"{x[0]}  (kind {x[1]} -> name it <verb>-<purpose>.yaml, not {x[2]}-*)"),
    ]
    problems = sum(len(items) for _, items, _ in groups)
    for title, items, fmt in groups:
        if items:
            print(f"\n✗ {title} ({len(items)}):")
            for it in sorted(items):
                print("   " + fmt(it))

    if problems:
        print(f"\n{problems} naming violation(s). See AGENTS.md 'File and Directory Naming Conventions'.")
        sys.exit(1)
    print("✓ All manifest naming conventions satisfied.")

if __name__ == "__main__":
    main()
