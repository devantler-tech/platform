#!/usr/bin/env bash
# Actual report/guard discovery must not silently omit a controller-created source.
set -euo pipefail
export LC_ALL=C
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
report="${REPORT_UNDER_TEST:-$root/scripts/report-publish-workflow-signing-revisions.sh}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/tree/k8s/apps"
input="$work/tree/k8s/apps/tenant.yaml"
failures=0
discover() {
  bash -c 'source "$1"; discover_consumers "$2"' bash "$report" "$work/tree"
}
refuses() {
  local name="$1"
  if discover >"$work/result" 2>"$work/error"; then
    printf 'FAIL: silently accepted %s\n' "$name" >&2
    failures=$((failures + 1))
  elif [ -s "$work/result" ]; then
    printf 'FAIL: partial inventory after %s\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

cat >"$work/good.yaml" <<'EOF'
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata:
  name: business-site
spec:
  source:
    reconcileApplication: false
EOF
cp "$work/good.yaml" "$input"
refuses 'literal source-only Tenant'
yq '.spec.source.reconcileApplication=true' "$work/good.yaml" >"$input"
refuses 'active Tenant'
yq -o=json '.' "$work/good.yaml" >"$input"
refuses 'JSON with quoted keys'
printf '{"apiVersion":"kro.run/v1alpha1","kind":"Tenant"}\n' >"$input"
refuses 'flow document'
printf '"apiVersion": "kro.run/v1alpha1"\n"kind": "Tenant"\n' >"$input"
refuses 'quoted YAML keys'
printf 'apiVersion: kro.run/v1alpha1\nkind: "Te\\x6eant"\n' >"$input"
refuses 'escaped kind spelling'
printf 'schema: &schema kro.run/v1alpha1\napiVersion: *schema\nkind: Tenant\n' >"$input"
refuses 'YAML alias'
printf 'schema: &schema\n  apiVersion: kro.run/v1alpha1\n  kind: Tenant\nobject:\n  <<: *schema\n' >"$input"
refuses 'YAML merge'
{ printf 'apiVersion: v1\nkind: ConfigMap\n---\n'; cat "$work/good.yaml"; } >"$input"
refuses 'second document'
printf 'apiVersion: example.test/v1\nkind: Carrier\nspec:\n  template:\n    apiVersion: kro.run/v1alpha1\n    kind: Tenant\n' >"$input"
refuses 'nested typed controller template'
cp "$work/good.yaml" "$input"
mv "$input" "${input%.yaml}.yml"
refuses 'yml filename'
rm "${input%.yaml}.yml"
printf 'apiVersion: [broken\n' >"$input"
refuses 'unreadable candidate syntax'

# Complete foreign GVKs are not assumed to use this source-producing constructor.
printf 'apiVersion: other.example/v1\nkind: Tenant\n' >"$input"
discover >"$work/result"
[ ! -s "$work/result" ]
# Comments/prose naming Tenant do not instantiate its schema.
printf 'apiVersion: v1\nkind: ConfigMap\ndata:\n  note: "Tenant kro.run/v1alpha1"\n' >"$input"
discover >"$work/result"
[ ! -s "$work/result" ]
# Existing local-only Docker scope stays outside the production raw census.
mkdir -p "$work/tree/k8s/providers/docker"
cp "$work/good.yaml" "$work/tree/k8s/providers/docker/local-tenant.yaml"
discover >"$work/result"
[ ! -s "$work/result" ]

# Direct document extraction (used on rendered roots) has the same refusal.
if bash -c 'source "$1"; consumer_rows "$2" with-identity' bash "$report" "$work/good.yaml" \
  >"$work/result" 2>"$work/error"; then
  printf 'FAIL: direct rendered reader omitted a typed source\n' >&2
  failures=$((failures + 1))
fi
printf '%s failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
