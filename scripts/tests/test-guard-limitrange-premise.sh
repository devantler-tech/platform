#!/usr/bin/env bash
#
# Pins the LimitRange-premise guard's verdict in all THREE directions.
#
# The guard's job is to make a suppression that rests on "a LimitRange supplies
# this at admission" impossible to keep in an overlay that ships no such
# LimitRange. The cases that matter are therefore:
#
#   exit 1  the premise is invoked, and the overlay shipping the file has no
#           LimitRange in that namespace — the real defect
#   exit 0  the premise is invoked and the overlay DOES ship one, INCLUDING when
#           the LimitRange is reached through a nested kustomization rather than
#           named directly (the graph step the bad inference skipped)
#   exit 2  the guard could not check — missing root, unparseable kustomization,
#           or a premised skip on a resource that states no namespace
#
# Two of these are anti-vacuity cases rather than behaviours: a tree with no
# premised suppression, and a suppression whose reason is about something else,
# must both come back clean WITHOUT being reported as "checked". A matcher that
# silently matched nothing would otherwise be indistinguishable from a healthy
# tree — which is the same failure the guard itself exists to prevent.
#
# Every fixture carries its OWN provider name, filename and namespace, so an
# assertion can only be satisfied by the case it belongs to.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-limitrange-premise.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <root>
  # Capture the status with `if` rather than toggling `set -e`: this file runs under
  # `set -uo pipefail` with NO errexit, so `set -e` here would ENABLE a mode the script
  # never had and let a later setup failure kill the run instead of being reported
  # through the assertion summary.
  if GUARD_OUT="$("$guard" "$1" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

# A LimitRange base, shared in shape by every fixture that has one.
make_limitrange_base() { # <root> <namespace>
  mkdir -p "$1/bases/limit-ranges"
  cat >"$1/bases/limit-ranges/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - range.yaml
YAML
  cat >"$1/bases/limit-ranges/range.yaml" <<YAML
apiVersion: v1
kind: LimitRange
metadata:
  name: default-limitrange
  namespace: $2
spec:
  limits:
    - type: Container
      default:
        cpu: "2"
YAML
}

# The Flux layer roots the guard seeds its walk from. Every fixture needs these:
# the guard reads clusters/base rather than hardcoding the layers, so a tree with
# no layer definition is "cannot determine what is deployed" (exit 2), not clean.
# Only the `infrastructure` layer is declared here because that is the path every
# fixture's provider overlay lives at; `bootstrap` is deliberately included with a
# cluster-scoped path so the fixtures also pin that a NON-provider layer is skipped
# rather than having a provider name substituted into it.
make_layer_roots() { # <root>
  mkdir -p "$1/clusters/base"
  cat >"$1/clusters/base/flux-kustomization-infrastructure.yaml" <<YAML
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: infrastructure
spec:
  path: providers/__PROVIDER__/infrastructure
YAML
  cat >"$1/clusters/base/flux-kustomization-bootstrap.yaml" <<YAML
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: bootstrap
spec:
  path: clusters/__CLUSTER__/bootstrap
YAML
}

# One provider shipping one annotated workload. <extra> is spliced into the
# overlay's resource list, so neighbouring cases differ in exactly one line.
make_provider() { # <root> <provider> <workload-basename> <namespace> <skip-reason> <extra-resource>
  local root=$1 prov=$2 base=$3 ns=$4 reason=$5 extra=$6
  make_layer_roots "$root"
  mkdir -p "$root/providers/$prov/infrastructure"
  {
    printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n'
    printf '  - %s.yaml\n' "$base"
    [ -n "$extra" ] && printf '  - %s\n' "$extra"
  } >"$root/providers/$prov/infrastructure/kustomization.yaml"
  cat >"$root/providers/$prov/infrastructure/$base.yaml" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $base
  namespace: $ns
  annotations:
    checkov.io/skip1: "$reason"
spec:
  replicas: 1
YAML
}

expect() { # <case> <expected-rc> <root> [<needle>]
  local case_name=$1 want=$2 root=$3 needle=${4-}
  assertions=$((assertions + 1))
  run_guard "$root"
  if [ "$GUARD_RC" -ne "$want" ]; then
    printf 'FAIL %s: expected exit %s, got %s\n%s\n' "$case_name" "$want" "$GUARD_RC" "$GUARD_OUT" >&2
    failures=$((failures + 1))
    return
  fi
  if [ -n "$needle" ] && ! printf '%s' "$GUARD_OUT" | grep -qF -- "$needle"; then
    printf 'FAIL %s: exit %s was right, but the report never named %s\n%s\n' \
      "$case_name" "$want" "$needle" "$GUARD_OUT" >&2
    failures=$((failures + 1))
    return
  fi
  printf 'ok   %s (exit %s)\n' "$case_name" "$want"
}

# --- 1. THE DEFECT: premise invoked, overlay ships no LimitRange -------------
missing="$scratch/missing"
make_provider "$missing" dockerlike unshielded-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" ""
make_limitrange_base "$missing" kube-system # present in the tree, but NOT referenced
expect 'premise-without-limitrange-fails' 1 "$missing" 'unshielded-dns'

# The report must name the overlay that lacks it, not merely the file: the fix
# lives in the kustomization, not in the annotated workload.
run_guard "$missing"
assertions=$((assertions + 1))
if printf '%s' "$GUARD_OUT" | grep -qF 'dockerlike' && printf '%s' "$GUARD_OUT" | grep -qF 'kube-system'; then
  printf 'ok   failure-report-names-provider-and-namespace\n'
else
  printf 'FAIL failure-report-names-provider-and-namespace\n%s\n' "$GUARD_OUT" >&2
  failures=$((failures + 1))
fi

# --- 2. THE FIX: same tree, overlay now references the base -----------------
shipped="$scratch/shipped"
make_provider "$shipped" hetznerlike shielded-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" \
  "../../../bases/limit-ranges/"
make_limitrange_base "$shipped" kube-system
expect 'premise-with-limitrange-passes' 0 "$shipped" 'shielded-dns'

# --- 3. The LimitRange reached only through a NESTED kustomization ----------
# The whole defect was an inference about reachability, so a guard that only
# looked one level deep would reproduce it.
nested="$scratch/nested"
make_provider "$nested" nestedlike deep-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" \
  "../../../bases/aggregate/"
make_limitrange_base "$nested" kube-system
mkdir -p "$nested/bases/aggregate"
cat >"$nested/bases/aggregate/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../limit-ranges/
YAML
expect 'limitrange-reached-through-nested-base' 0 "$nested" 'deep-dns'

# --- 4. WRONG NAMESPACE: a LimitRange that does not cover this resource -----
# Shipping *a* LimitRange is not the claim; shipping one into the annotated
# resource's namespace is.
wrongns="$scratch/wrongns"
make_provider "$wrongns" otherns-like misplaced-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" \
  "../../../bases/limit-ranges/"
make_limitrange_base "$wrongns" some-other-namespace
expect 'limitrange-in-another-namespace-fails' 1 "$wrongns" 'misplaced-dns'

# --- 5. ANTI-VACUITY: a skip whose reason is about something else -----------
unrelated="$scratch/unrelated"
make_provider "$unrelated" unrelatedlike plain-dns kube-system \
  "CKV_K8S_38=CoreDNS resolves cluster DNS by watching Services via the API server" ""
expect 'unrelated-skip-is-not-matched' 0 "$unrelated" 'no suppression'

# --- 6. ANTI-VACUITY: no suppression at all --------------------------------
bare="$scratch/bare"
make_layer_roots "$bare"
mkdir -p "$bare/providers/barelike/infrastructure"
cat >"$bare/providers/barelike/infrastructure/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
YAML
expect 'empty-tree-reports-nothing-checked' 0 "$bare" 'no suppression'

# --- 7. COULD-NOT-CHECK: unparseable kustomization -------------------------
broken="$scratch/broken"
make_provider "$broken" brokenlike broken-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" ""
printf 'resources: [ this is not\n  valid: yaml: at all\n' \
  >"$broken/providers/brokenlike/infrastructure/kustomization.yaml"
expect 'unparseable-kustomization-is-exit-2' 2 "$broken" 'brokenlike'

# --- 8. COULD-NOT-CHECK: no providers directory ----------------------------
noprov="$scratch/noprov"
mkdir -p "$noprov/bases"
expect 'missing-providers-dir-is-exit-2' 2 "$noprov" 'providers'

# --- 9. COULD-NOT-CHECK: premised skip on a namespace-less resource --------
nons="$scratch/nons"
make_layer_roots "$nons"
mkdir -p "$nons/providers/nonslike/infrastructure"
cat >"$nons/providers/nonslike/infrastructure/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespaceless.yaml
YAML
cat >"$nons/providers/nonslike/infrastructure/namespaceless.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: namespaceless
  annotations:
    checkov.io/skip1: "CKV_K8S_11=the default-limitrange supplies the CPU limit at admission"
spec:
  replicas: 1
YAML
expect 'premised-skip-without-namespace-is-exit-2' 2 "$nons" 'namespaceless'

# --- 10. COULD-NOT-CHECK: a reachable LimitRange candidate that will not parse ---
# A parse failure is an inability to check. Skipping it would let the file contribute
# no namespace, and the guard would then report a DEFINITE premise violation over input
# it could not read — a wrong verdict, not a missing one.
badrange="$scratch/badrange"
make_provider "$badrange" badrangelike unparseable-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" \
  "../../../bases/limit-ranges/"
make_limitrange_base "$badrange" kube-system
printf 'apiVersion: v1\nkind: [ LimitRange\n  bad: yaml: here\n' >"$badrange/bases/limit-ranges/range.yaml"
expect 'unparseable-reachable-limitrange-is-exit-2' 2 "$badrange" 'range.yaml'

# --- 11. COULD-NOT-CHECK: a kustomize namespace transformer -----------------
# `namespace:` rewrites every resource below it, so the namespace read from the file is
# no longer the one that must match. The guard refuses rather than comparing the wrong one.
nsxform="$scratch/nsxform"
make_provider "$nsxform" nsxformlike transformed-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" \
  "../../../bases/limit-ranges/"
make_limitrange_base "$nsxform" kube-system
printf 'namespace: somewhere-else\n' >>"$nsxform/providers/nsxformlike/infrastructure/kustomization.yaml"
expect 'namespace-transformer-is-exit-2' 2 "$nsxform" 'namespace transformer'

# --- 12. An ABSOLUTE resources entry still resolves to the file it names ------
# `canonicalize` joins every entry onto the naming kustomization's directory. An entry
# that is already absolute must not be joined — the combined path names nothing, the graph
# walk misses the LimitRange it does reach, and the guard reports a premise violation that
# is not there. That is a FALSE exit 1: the most expensive verdict this guard can give,
# because it fails a build over a suppression that is in fact well-founded.
absentry="$scratch/absentry"
make_provider "$absentry" absentrylike absolute-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" \
  "$scratch/absentry/bases/limit-ranges/"
make_limitrange_base "$absentry" kube-system
expect 'absolute-resources-entry-resolves' 0 "$absentry" 'absolute-dns'

# --- 13. A MULTI-DOCUMENT file resolves the namespace PER DOCUMENT -----------
# `values` is read from every document in the file, but the namespace was taken from
# the FIRST one. A file whose annotated document is not document 0 — the ordinary shape
# of an operator bundle, and already the shape of cdi-operator.yaml and
# kubevirt-operator.yaml in this repo — is then checked against the wrong namespace, or
# against no namespace at all when document 0 states none. Both readings are wrong about
# a file the guard reports as checked.
#
# Here document 0 is a namespaceless cluster-scoped object and the annotated Deployment
# in document 1 states `multidoc-ns`, where the LimitRange is shipped. The correct
# verdict is exit 0.
multidoc="$scratch/multidoc"
make_layer_roots "$multidoc"
mkdir -p "$multidoc/providers/multidoclike/infrastructure"
{
  printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n'
  printf '  - bundle-dns.yaml\n'
  printf '  - %s\n' "$multidoc/bases/limit-ranges/"
} >"$multidoc/providers/multidoclike/infrastructure/kustomization.yaml"
cat >"$multidoc/providers/multidoclike/infrastructure/bundle-dns.yaml" <<'YAML'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: bundle-dns-reader
rules: []
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bundle-dns
  namespace: multidoc-ns
  annotations:
    checkov.io/skip1: "CKV_K8S_11=the multidoc-ns default-limitrange supplies the CPU limit at admission"
spec:
  replicas: 1
YAML
make_limitrange_base "$multidoc" multidoc-ns
expect 'multidoc-namespace-resolves-per-document' 0 "$multidoc" 'bundle-dns'

# --- 14. A LimitRange that supplies no default CPU does NOT satisfy the premise ---
# The suppressed control is CKV_K8S_11 ("CPU limits should be set") and the reason
# says the limit arrives at admission. Only `.spec.limits[].default.cpu` makes that
# true: a range carrying only `defaultRequest`, only `max`, or only a memory default
# leaves admission supplying no CPU limit at all, so the suppression is unfounded
# while a namespace-only check still calls it satisfied. That is the fail-open this
# guard exists to prevent, one field in.
capless="$scratch/capless"
make_provider "$capless" caplessprov capless-dns capless-ns \
  'CKV_K8S_11=the capless-ns default-limitrange supplies the CPU limit at admission' \
  '../../../bases/limit-ranges/'
mkdir -p "$capless/bases/limit-ranges"
cat >"$capless/bases/limit-ranges/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - range.yaml
YAML
cat >"$capless/bases/limit-ranges/range.yaml" <<YAML
apiVersion: v1
kind: LimitRange
metadata:
  name: default-limitrange
  namespace: capless-ns
spec:
  limits:
    - type: Container
      defaultRequest:
        cpu: 15m
      max:
        memory: 1Gi
YAML
expect 'limitrange-without-default-cpu-fails' 1 "$capless" 'capless-dns'
# --- 16. THE DEFECT THIS FIX CLOSES: a LimitRange in an UNREFERENCED subtree ---
# The overlay ships the annotated workload but does NOT reference limit-ranges/.
# `find` reaches that subtree's kustomization.yaml, the Flux layer graph does not.
# Seeding from `find` therefore credited the provider with a LimitRange it never
# deploys and PASSED — a fail-open suppression. Seeded from the layer root, the
# subtree is correctly invisible and the premise is reported as unshielded.
unref="$scratch/unref"
make_provider "$unref" unreflike orphan-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" ""
mkdir -p "$unref/providers/unreflike/infrastructure/opt-in"
cat >"$unref/providers/unreflike/infrastructure/opt-in/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - range.yaml
YAML
cat >"$unref/providers/unreflike/infrastructure/opt-in/range.yaml" <<'YAML'
apiVersion: v1
kind: LimitRange
metadata:
  name: default-limitrange
  namespace: kube-system
spec:
  limits:
    - type: Container
      default:
        cpu: "2"
YAML
expect 'unreferenced-subtree-limitrange-does-not-shield' 1 "$unref" 'orphan-dns'

# --- 17. THE PAIRED CONTROL: the SAME subtree, now REFERENCED, does shield ----
# Differs from case 16 in exactly one line — the overlay's `resources:` entry — so
# a pass here can only be attributed to reachability, not to the fixture shape.
refd="$scratch/refd"
make_provider "$refd" refdlike adopted-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" "opt-in"
mkdir -p "$refd/providers/refdlike/infrastructure/opt-in"
cat >"$refd/providers/refdlike/infrastructure/opt-in/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - range.yaml
YAML
cat >"$refd/providers/refdlike/infrastructure/opt-in/range.yaml" <<'YAML'
apiVersion: v1
kind: LimitRange
metadata:
  name: default-limitrange
  namespace: kube-system
spec:
  limits:
    - type: Container
      default:
        cpu: "2"
YAML
expect 'referenced-subtree-limitrange-does-shield' 0 "$refd" 'adopted-dns'

# --- 18. COULD-NOT-CHECK: no Flux layer definitions to seed from -------------
# Without clusters/base the guard cannot know which paths a provider deploys.
# That must be exit 2, never a clean 0: an unknown deployment surface and a clean
# tree would otherwise be indistinguishable — the property this guard's header
# calls out explicitly.
nolayer="$scratch/nolayer"
make_provider "$nolayer" nolayerlike blind-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" ""
rm -rf "$nolayer/clusters"
expect 'missing-layer-definitions-is-exit-2' 2 "$nolayer" 'flux-kustomization'


# --- 19. A CLUSTER-SCOPED layer (bootstrap) also shields ---------------------
# The bootstrap layer root is `clusters/__CLUSTER__/bootstrap`, not
# `providers/__PROVIDER__/...`, yet it reaches a provider overlay one hop in. The
# guard skipped every cluster-scoped layer, so a LimitRange shipped there was
# invisible and its premised suppression was reported as unshielded — a FALSE
# violation that fails the build on correct work (#3271).
boot="$scratch/boot"
make_provider "$boot" bootlike boot-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" ""
mkdir -p "$boot/clusters/one/bootstrap" "$boot/providers/bootlike/bootstrap/limit-ranges"
cat >"$boot/clusters/one/bootstrap/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../providers/bootlike/bootstrap
YAML
cat >"$boot/providers/bootlike/bootstrap/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - limit-ranges/
YAML
cat >"$boot/providers/bootlike/bootstrap/limit-ranges/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - range.yaml
YAML
cat >"$boot/providers/bootlike/bootstrap/limit-ranges/range.yaml" <<'YAML'
apiVersion: v1
kind: LimitRange
metadata:
  name: default-limitrange
  namespace: kube-system
spec:
  limits:
    - type: Container
      default:
        cpu: "2"
YAML
expect 'bootstrap-layer-limitrange-does-shield' 0 "$boot" 'boot-dns'

# --- 20. THE PAIRED FAIL-OPEN CONTROL: another provider's bootstrap does NOT --
# Widening to cluster-scoped layers is only safe if a cluster root seeds the ONE
# provider its own graph reaches. Seeding every cluster into every provider would
# let one provider's LimitRange satisfy another's suppression — fail-OPEN, the
# direction this guard exists to avoid. Differs from case 19 in exactly one line:
# the cluster tree points at `otherlike`, while the suppression is `bootlike`'s.
xprov="$scratch/xprov"
make_provider "$xprov" bootlike lonely-dns kube-system \
  "CKV_K8S_11=the kube-system default-limitrange supplies the CPU limit at admission" ""
mkdir -p "$xprov/clusters/one/bootstrap" "$xprov/providers/otherlike/bootstrap/limit-ranges"
cat >"$xprov/clusters/one/bootstrap/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../providers/otherlike/bootstrap
YAML
cat >"$xprov/providers/otherlike/bootstrap/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - limit-ranges/
YAML
cat >"$xprov/providers/otherlike/bootstrap/limit-ranges/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - range.yaml
YAML
cat >"$xprov/providers/otherlike/bootstrap/limit-ranges/range.yaml" <<'YAML'
apiVersion: v1
kind: LimitRange
metadata:
  name: default-limitrange
  namespace: kube-system
spec:
  limits:
    - type: Container
      default:
        cpu: "2"
YAML
expect 'other-providers-bootstrap-limitrange-does-not-shield' 1 "$xprov" 'lonely-dns'
printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ] || exit 1
