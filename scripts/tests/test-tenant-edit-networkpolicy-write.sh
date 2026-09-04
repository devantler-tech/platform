#!/usr/bin/env bash
# The tenant identity must hold NO write verb on standard NetworkPolicy (#3396).
#
# WHY THIS EXISTS
# restrict-tenant-network-policies constrains what a tenant may express in a
# CiliumNetworkPolicy, and Cilium unions standard NetworkPolicy allow rules with it
# additively. A tenant able to create a `networking.k8s.io/v1` NetworkPolicy with
# `podSelector: {}`, `ingress: [{}]`, `egress: [{}]` reopens every path the boundary
# closes, and the CiliumNetworkPolicy webhook never sees it. Every tenant expresses its
# allow-list as a CiliumNetworkPolicy (the scaffold and both adopted tenants), so the
# write grant is capability nobody uses and one bypass everybody could — it is removed
# from tenant-base-edit, and this test keeps it removed.
#
# WHAT IS PROVED
# Both providers' infrastructure layers are RENDERED (`kubectl kustomize`), so the
# assertion covers what ships rather than one file: every ClusterRole carrying the
# `devantler.tech/aggregate-to-tenant-edit: "true"` label — the whole set aggregated
# into `tenant-edit` — is checked, and any rule that grants a write verb on
# `networkpolicies` in `networking.k8s.io` (a `*` group, resource or verb counts as
# the grant) fails by name. Read verbs stay: a tenant may inspect the generated
# default-deny in its own namespace.
#
# A floor guards the filter: at least one labelled ClusterRole must render, or an
# empty selection would report a clean overlay while checking nothing. The ablation
# below then feeds a synthetic rendered stream carrying the grant through the same
# checker and requires the refusal, so the checker is proven to see the shape it
# exists to refuse rather than passing on a scan that matched nothing.
#
# SEAM (for the ablation): TENANT_EDIT_RENDER_FILE replaces the rendered overlay.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

command -v yq >/dev/null 2>&1 || { printf 'yq is required\n' >&2; exit 1; }
command -v kubectl >/dev/null 2>&1 || { printf 'kubectl is required to render the overlays\n' >&2; exit 1; }

readonly LABEL='devantler.tech/aggregate-to-tenant-edit'

# Rows: <name>\t<apiGroups>\t<resources>\t<verbs> for every rule of every labelled
# ClusterRole, lists joined with commas. Rendered as one stream per overlay.
# The label is spliced in through a quote break; the surrounding yq program is
# deliberately single-quoted so its own `$name` binding is not shell-expanded.
# shellcheck disable=SC2016
readonly YQ_ROWS='select(.kind == "ClusterRole" and (.metadata.labels["'"$LABEL"'"] // "") == "true")
  | .metadata.name as $name
  | (.rules // [])[]
  | [$name, ((.apiGroups // []) | join(",")), ((.resources // []) | join(",")), ((.verbs // []) | join(","))]
  | @tsv'

readonly YQ_LABELLED_COUNT='[select(.kind == "ClusterRole" and (.metadata.labels["'"$LABEL"'"] // "") == "true")] | length'

has_item() { # has_item <comma-list> <item>
  case ",$1," in *",$2,"*) return 0 ;; esac
  return 1
}

# check_stream <rendered-yaml-file> <label-for-messages> — exit 1 naming each grant.
check_stream() {
  local file="$1" what="$2" labelled status=0
  labelled="$(yq -r "$YQ_LABELLED_COUNT" "$file" | awk '{s+=$1} END {print s+0}')"
  if [ "$labelled" -lt 1 ]; then
    printf '%s: no ClusterRole carries %s: "true" in the render — the selection matched nothing, so this check would pass vacuously\n' "$what" "$LABEL" >&2
    return 1
  fi
  local name groups resources verbs
  while IFS=$'\t' read -r name groups resources verbs; do
    [ -n "$name" ] || continue
    if has_item "$groups" 'networking.k8s.io' || has_item "$groups" '*'; then
      if has_item "$resources" 'networkpolicies' || has_item "$resources" '*'; then
        local v
        for v in create update patch delete deletecollection '*'; do
          if has_item "$verbs" "$v"; then
            printf '%s: ClusterRole %s (aggregated into tenant-edit) grants %s on networking.k8s.io/networkpolicies — a tenant could submit a standard NetworkPolicy that reopens every path restrict-tenant-network-policies closes (#3396)\n' \
              "$what" "$name" "$v" >&2
            status=1
            break
          fi
        done
      fi
    fi
  done < <(yq -r "$YQ_ROWS" "$file")
  return "$status"
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# ── Ablation first: the checker must refuse the grant it exists to refuse ────────────
cat >"$work/ablation.yaml" <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: synthetic-tenant-piece
  labels:
    $LABEL: "true"
rules:
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses", "networkpolicies"]
    verbs: ["get", "list", "watch", "create"]
EOF
if check_stream "$work/ablation.yaml" 'ablation' 2>"$work/ablation.err"; then
  printf 'FAIL: the checker accepted a synthetic ClusterRole granting create on networkpolicies; it cannot see the shape it exists to refuse\n' >&2
  exit 1
fi
grep -q 'synthetic-tenant-piece (aggregated into tenant-edit) grants create' "$work/ablation.err" || {
  printf 'FAIL: the ablation was refused, but not for the right reason:\n' >&2
  cat "$work/ablation.err" >&2
  exit 1
}
# ...and a read-only grant is NOT refused, so the check cannot pass by refusing everything.
cat >"$work/control.yaml" <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: synthetic-reader
  labels:
    $LABEL: "true"
rules:
  - apiGroups: ["networking.k8s.io"]
    resources: ["networkpolicies"]
    verbs: ["get", "list", "watch"]
EOF
check_stream "$work/control.yaml" 'control' || {
  printf 'FAIL: the checker refused a read-only networkpolicies grant; read access is meant to stay\n' >&2
  exit 1
}
printf 'ok: ablation refused by name, read-only control accepted\n'

# ── The real overlays ────────────────────────────────────────────────────────────────
status=0
if [ -n "${TENANT_EDIT_RENDER_FILE:-}" ]; then
  check_stream "$TENANT_EDIT_RENDER_FILE" "$TENANT_EDIT_RENDER_FILE" || status=1
else
  # The cluster overlays under k8s/clusters/ render only the Flux wiring; the
  # ClusterRoles live in the infrastructure layer each provider overlay assembles,
  # which is what Flux applies. Render that layer for both providers.
  for overlay in providers/docker/infrastructure providers/hetzner/infrastructure; do
    slug="${overlay//\//-}"
    if ! kubectl kustomize "$repo_root/k8s/$overlay/" >"$work/$slug.yaml" 2>"$work/$slug.err"; then
      printf 'FAIL: kubectl kustomize k8s/%s failed:\n' "$overlay" >&2
      cat "$work/$slug.err" >&2
      exit 1
    fi
    if check_stream "$work/$slug.yaml" "k8s/$overlay"; then
      printf 'ok: k8s/%s — no tenant-aggregated ClusterRole grants a write verb on networkpolicies\n' "$overlay"
    else
      status=1
    fi
  done
fi
exit "$status"
