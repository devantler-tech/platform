#!/usr/bin/env bash
# Actual report/guard discovery must not silently omit a controller-created source.
set -euo pipefail
export LC_ALL=C
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
report="${REPORT_UNDER_TEST:-$root/scripts/report-publish-workflow-signing-revisions.sh}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/tree/k8s/apps"
cat >"$work/tree/k8s/controller.yaml" <<'EOF'
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: tenant.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: Tenant
  resources:
    - id: source
      template:
        apiVersion: source.toolkit.fluxcd.io/v1
        kind: OCIRepository
        spec: {}
EOF
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
for extension in json YAML YML JSON; do
  cp "$work/good.yaml" "$work/tree/k8s/apps/source.$extension"
  refuses "$extension resource filename"
  rm "$work/tree/k8s/apps/source.$extension"
done
mkdir -p "$work/tree/k8s/targets"
cp "$work/good.yaml" "$work/tree/k8s/targets/typed.txt"
ln -s ../targets/typed.txt "$input"
refuses 'contained manifest file link'
rm "$input"
ln -s ../targets "$work/tree/k8s/apps/linked"
refuses 'linked directory'
rm "$work/tree/k8s/apps/linked"
ln -s "$work/good.yaml" "$input"
refuses 'manifest link outside published k8s root'
rm "$input"
ln -s missing "$input"
refuses 'dangling manifest link'
rm "$input"
printf 'apiVersion: [broken\n' >"$input"
refuses 'unreadable candidate syntax'

# Complete foreign GVKs are not assumed to use this source-producing constructor.
printf 'apiVersion: other.example/v1\nkind: Tenant\n' >"$input"
discover >"$work/result"
[ ! -s "$work/result" ]
# A contained ordinary manifest link retains the existing artifact boundary.
cp "$input" "$work/tree/k8s/targets/ordinary.txt"
ln -s ../targets/ordinary.txt "$work/tree/k8s/apps/ordinary.yaml"
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

# The declared schema, not the name Tenant alone, defines a source instance.
yq -i '.spec.schema.group="tenants.example.test"' "$work/tree/k8s/controller.yaml"
cp "$work/good.yaml" "$input"
discover >"$work/result"
[ ! -s "$work/result" ]
yq '.apiVersion="tenants.example.test/v1alpha1"' "$work/good.yaml" >"$input"
refuses 'matching custom-group instance'
yq -i 'del(.spec.schema.apiVersion)' "$work/tree/k8s/controller.yaml"
refuses 'source-producing schema without a complete GVK'
printf '%s failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
