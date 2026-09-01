#!/usr/bin/env bash
#
# Fail when a suppression is justified by "a LimitRange supplies it at admission"
# in an overlay that does not actually ship a LimitRange into that namespace.
#
# THE PREMISE THIS PROTECTS. Several suppressions in this tree are dispositioned not
# as "we accept the risk" but as "the check is blind to an admission-time default" —
# the kube-system `default-limitrange` supplies a CPU limit that the stored spec does
# not state, so checkov, reading the stored spec, reports a finding that is not real.
# That reasoning is sound, and it is only sound WHERE THE LIMITRANGE ACTUALLY APPLIES.
#
# WHY A GUARD RATHER THAN REVIEW. The premise fails silently and asymmetrically. A
# LimitRange lives in its own base, so the natural way to justify one of these skips
# is "it is a base resource, therefore it applies to every overlay" — an inference
# about REACHABILITY that reads as obviously true and is not. An overlay that
# cherry-picks named base directories, rather than pulling the aggregate, does not
# get it. Nothing then fails: the suppression keeps suppressing, the scan stays
# green, and the workload runs with no limit in exactly the overlay whose stated
# goal was prod-like admission. The suppression still reads as reviewed.
#
# WHAT IT CHECKS. For every file carrying a `checkov.io/skipN` whose reason invokes
# the LimitRange-at-admission premise, this walks the kustomize graph of each
# provider that actually ships that file and asserts that the same provider also
# ships a LimitRange into the annotated resource's namespace.
#
# WHY REACHABILITY RATHER THAN A RENDER. `kustomize build` is the more direct
# question, but many roots here pull remote Helm charts and OCI bases, so a
# render-based guard would need the network and would exit 2 — "could not check" —
# far more often than it would answer. The graph walk is the same question asked
# statically: it resolves `resources:` and `components:` exactly as the overlay
# declares them, which is the precise step the bad inference skipped.
#
# Usage: guard-limitrange-premise.sh [root]        (default: k8s)
# Exit:  0 every LimitRange-premised suppression is in an overlay that ships one
#        1 at least one is not (each is named, with the overlay that lacks it)
#        2 the guard could not check — missing tool, unreadable root, parse failure
#
# Exit 2 is deliberately distinct from 0. A guard that cannot run must never be
# indistinguishable from a clean tree.

set -uo pipefail

root=${1:-k8s}

die() {
  printf 'guard-limitrange-premise: %s\n' "$1" >&2
  exit 2
}

command -v yq >/dev/null 2>&1 || die 'yq is required but not on PATH'
[ -d "$root" ] || die "not a directory: $root"
[ -d "$root/providers" ] || die "no providers directory under $root"

# Resolve a `resources:`/`components:` entry against the directory of the
# kustomization that named it. Entries that leave the tree (remote bases, OCI, URLs)
# are not filesystem paths and are skipped by the caller.
canonicalize() { # <dir> <entry>
  local base=$1 entry=$2 combined
  # An entry that is already absolute names its target outright. Joining it onto $base
  # would build a path that names nothing, and the graph walk would then miss a
  # LimitRange it does reach — a FALSE premise violation, which fails the build.
  case $entry in
    /*) combined=$entry ;;
    *) combined="$base/$entry" ;;
  esac
  # No realpath dependency: collapse `.`/`..` textually so the result is stable
  # whether or not the path exists (a dangling entry must not abort the walk).
  printf '%s' "$combined" | awk -F/ -v abs="${combined:0:1}" '{
    n = 0
    for (i = 1; i <= NF; i++) {
      if ($i == "" || $i == ".") continue
      if ($i == "..") { if (n > 0) n--; continue }
      parts[++n] = $i
    }
    out = ""
    for (i = 1; i <= n; i++) out = out "/" parts[i]
    # Preserve an absolute root: dropping the leading slash would make the path
    # unmatchable against the annotated-file list and read as "no overlay ships it".
    if (abs == "/") print out; else print substr(out, 2)
  }'
}

# Every filesystem path a provider's kustomize graph reaches, one per line.
reachable_files() { # <provider-dir>
  local provider=$1 queue seen_k="" files="" k dir entry target
  local lroot lpath lfile seeded=0 layer_defs=0
  # SEED FROM THE FLUX LAYER ROOTS, NEVER FROM `find`.
  #
  # Flux does not apply "every kustomization under the provider" — each layer
  # Kustomization in clusters/base targets ONE path, and a subtree that no layer
  # root reaches through its resources/components graph is NOT deployed. Seeding
  # with `find` swept those in, and because extra subtrees can only ADD LimitRange
  # namespaces, the error was fail-open: an unreferenced opt-in overlay could
  # supply the only LimitRange for a premised skip and the guard would approve a
  # suppression nothing actually shields. `providers/docker/infrastructure/
  # controllers/minio/` is exactly that shape — referenced only from comments.
  #
  # The roots are READ from clusters/base rather than hardcoded, so adding a layer
  # widens this guard automatically instead of silently narrowing it.
  for lfile in "$root"/clusters/base/flux-kustomization-*.yaml; do
    [ -f "$lfile" ] || continue
    layer_defs=$((layer_defs + 1))
    lpath=$(yq -r '.spec.path // ""' "$lfile" 2>/dev/null) ||
      die "could not parse layer definition $lfile"
    [ -n "$lpath" ] || continue
    # Only provider-scoped layers seed a provider walk. `bootstrap` targets
    # clusters/__CLUSTER__/bootstrap — a different tree that ships no provider
    # overlay — so substituting a provider name into it would name nothing.
    case $lpath in
      providers/__PROVIDER__ | providers/__PROVIDER__/*) ;;
      *) continue ;;
    esac
    lroot="$root/${lpath//__PROVIDER__/$(basename "$provider")}"
    [ -f "$lroot/kustomization.yaml" ] || continue
    queue="${queue}$lroot/kustomization.yaml"$'\n'
    seeded=$((seeded + 1))
  done
  # A guard that cannot find the layer definitions cannot know what is deployed,
  # and must not report a clean tree. Distinguish the two causes: no definitions
  # at all is a broken/unknown checkout, while definitions that name no existing
  # path for THIS provider is a provider that ships nothing through any layer.
  if [ "$layer_defs" -eq 0 ]; then
    die "no clusters/base/flux-kustomization-*.yaml layer definitions under $root; cannot determine what each provider deploys"
  fi
  [ "$seeded" -gt 0 ] || return 0
  while [ -n "$queue" ]; do
    k=${queue%%$'\n'*}
    if [ "$k" = "$queue" ]; then queue=""; else queue=${queue#*$'\n'}; fi
    [ -n "$k" ] || continue
    case $'\n'"$seen_k"$'\n' in *$'\n'"$k"$'\n'*) continue ;; esac
    seen_k="$seen_k$k"$'\n'
    dir=$(dirname "$k")
    # A `namespace:` transformer rewrites the namespace of every resource below this
    # kustomization, so `.metadata.namespace` read from the file would no longer be the
    # namespace the LimitRange must match. Resolving that properly means reimplementing
    # kustomize; refusing to answer is the honest alternative to answering wrongly.
    ns_xform=$(yq -r '.namespace // ""' "$k" 2>/dev/null) || die "could not parse $k"
    [ -z "$ns_xform" ] ||
      die "$k sets a kustomize namespace transformer ($ns_xform); this guard cannot resolve the effective namespace"
    # yq exits non-zero on a malformed kustomization; that is a real inability to
    # check the graph, not a clean overlay.
    entry=$(yq -r '(.resources // []) + (.components // []) | .[]' "$k" 2>/dev/null) ||
      die "could not parse $k"
    while IFS= read -r e; do
      [ -n "$e" ] || continue
      # Remote bases are not filesystem paths; a LimitRange cannot be asserted
      # through one statically, and none of this tree's limit ranges come that way.
      case $e in *"://"* | "git@"*) continue ;; esac
      target=$(canonicalize "$dir" "$e")
      if [ -d "$target" ]; then
        if [ -f "$target/kustomization.yaml" ]; then
          queue="$queue$target/kustomization.yaml"$'\n'
        fi
      elif [ -f "$target" ]; then
        files="$files$target"$'\n'
      fi
    done <<EOF
$entry
EOF
  done
  printf '%s' "$files"
}

# The reason text that marks a suppression as resting on this premise. Matched on
# the ANNOTATION VALUE rather than the neighbouring YAML comment: the value is the
# part checkov actually carries, it is what survives a reformat, and the sibling
# guard already forces it to be a quoted scalar carrying the whole reason.
premise_re='limit[ -]?range'

# Namespaces a provider ships a CPU-defaulting LimitRange into.
limitrange_namespaces() { # <files...on stdin>
  local f ns
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    case $f in *.yaml | *.yml) ;; *) continue ;; esac
    # A file may hold several documents; only LimitRange ones contribute — and only those
    # that actually supply the suppressed field. The premise these skips invoke is that a
    # CPU limit arrives at admission, which is true only of a Container-type limit carrying
    # `default.cpu`. A range with only `defaultRequest`, only `max`, or only a memory default
    # leaves the container with no CPU limit, so counting it would satisfy the premise with a
    # resource that does not supply it.
    # A parse failure here is an INABILITY TO CHECK, not an absent LimitRange. Skipping
    # it would let a malformed file contribute no namespace, and the caller would then
    # report a definite premise violation (exit 1) over input it could not read.
    ns=$(yq -r 'select(.kind == "LimitRange") | select([.spec.limits[] | select(.type == "Container" and .default.cpu != null)] | length > 0) | .metadata.namespace // ""' "$f" 2>/dev/null) ||
      die "could not parse a reachable LimitRange candidate: $f"
    while IFS= read -r n; do
      [ -n "$n" ] && printf '%s\n' "$n"
    done <<EOF
$ns
EOF
  done
  # An explicit success: a `while` loop returns the status of its LAST body command, so a
  # final line contributing no namespace would otherwise make this function look like it
  # failed — and the caller now treats that failure as fatal.
  return 0
}

annotated=$(grep -rlE 'checkov\.io/skip[0-9]+' --include='*.yaml' --include='*.yml' -- "$root" 2>/dev/null)
grep_rc=$?
if [ "$grep_rc" -gt 1 ]; then
  die "could not search $root for annotations (grep exit $grep_rc)"
fi

providers=$(find "$root/providers" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort)
[ -n "$providers" ] || die "no provider overlays under $root/providers"

# Precompute each provider's reachable file set and LimitRange namespaces once.
prov_files_dir=$(mktemp -d) || die 'could not create scratch dir'
trap 'rm -rf "$prov_files_dir"' EXIT
while IFS= read -r p; do
  [ -n "$p" ] || continue
  name=$(basename "$p")
  reachable_files "$p" >"$prov_files_dir/$name.files"
  # Deliberately NOT piped into `sort`: `die` inside a pipeline exits only the subshell,
  # and the pipeline then reports `sort`'s status, so a fatal parse error would vanish.
  limitrange_namespaces <"$prov_files_dir/$name.files" >"$prov_files_dir/$name.raw" ||
    die "could not collect LimitRange namespaces for provider $name"
  sort -u <"$prov_files_dir/$name.raw" >"$prov_files_dir/$name.ns" ||
    die "could not sort LimitRange namespaces for provider $name"
done <<EOF
$providers
EOF

failures=0
checked=0

while IFS= read -r file; do
  [ -n "$file" ] || continue
  # ONE RECORD PER YAML DOCUMENT, pairing that document's namespace with that document's
  # own premise-bearing annotation values. Reading the values from every document while
  # taking the namespace from only the first checks a later document against the wrong
  # namespace — or, when document 0 is a namespaceless cluster-scoped object, against no
  # namespace at all, which aborts the build on a well-formed file. That is the ordinary
  # shape of an operator bundle and already the shape of cdi-operator.yaml and
  # kubevirt-operator.yaml here.
  # `@tsv` escapes any tab or newline inside a value, so one document is always one line.
  records=$(yq -r '[(.metadata.namespace // ""), ((.metadata.annotations // {}) | to_entries | map(select(.key | test("^checkov\\.io/skip[0-9]+$")) | .value) | join(" "))] | @tsv' \
    "$file" 2>/dev/null) || die "could not read annotations from $file"

  doc=-1
  while IFS= read -r record; do
    doc=$((doc + 1))
    # Split on the first tab with parameter expansion, NOT `IFS=$'\t' read`: tab is an IFS
    # whitespace character, so `read` would collapse an empty leading namespace field and
    # shift the values into it — silently checking against a namespace named after a reason.
    ns=${record%%$'\t'*}
    values=${record#*$'\t'}
    printf '%s' "$values" | grep -qiE "$premise_re" || continue
    [ -n "$ns" ] ||
      die "$file document $doc carries a LimitRange-premised skip but states no namespace"

    shipped_by=0
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      name=$(basename "$p")
      grep -qxF -- "$file" "$prov_files_dir/$name.files" || continue
      shipped_by=$((shipped_by + 1))
      checked=$((checked + 1))
      if grep -qxF -- "$ns" "$prov_files_dir/$name.ns"; then
        printf 'ok   %s — provider %s ships a CPU-defaulting LimitRange into %s\n' "$file" "$name" "$ns"
      else
        printf 'FAIL %s\n' "$file" >&2
        printf '     its skip reason rests on a LimitRange applying at admission, but provider\n' >&2
        printf '     %s ships NO LimitRange supplying a default CPU limit into\n' "$name" >&2
        printf '     namespace %s. A LimitRange that sets only defaultRequest, only max,\n' "$ns" >&2
        printf '     or only a memory default does not supply one.\n' >&2
        printf '     Fix: add (or extend) a Container-type default.cpu in that overlay'"'"'s\n' >&2
        printf '     limit-ranges base, or drop the skip and state the limit on the workload.\n' >&2
        failures=$((failures + 1))
      fi
    done <<EOF
$providers
EOF

    if [ "$shipped_by" -eq 0 ]; then
      # An unattributable premised suppression is NOT benign. Either the walk failed
      # to reach a file that is really shipped, or the suppression is dead code — and
      # from here those are indistinguishable. Both mean the premise went unchecked,
      # which is precisely the silent pass this guard exists to refuse.
      die "$file document $doc carries a LimitRange-premised skip, but no provider overlay reaches it"
    fi
  done <<EOF
$records
EOF
done <<EOF
$annotated
EOF

if [ "$checked" -eq 0 ]; then
  # No premised suppression anywhere is a legitimately clean tree, but it is also
  # what a broken matcher looks like. Say which it is rather than printing "ok".
  printf 'limitrange premise: no suppression under %s invokes the premise\n' "$root"
  exit 0
fi

if [ "$failures" -gt 0 ]; then
  printf '\nlimitrange premise: %d premised suppression(s) rest on an overlay that ships none.\n' "$failures" >&2
  exit 1
fi

printf '\nlimitrange premise: %d premised suppression(s) checked, all shipped with a CPU-defaulting LimitRange.\n' "$checked"
exit 0
