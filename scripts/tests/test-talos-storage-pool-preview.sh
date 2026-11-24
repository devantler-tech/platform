#!/usr/bin/env bash
set -euo pipefail
umask 077

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf -- "$scratch"' EXIT
cd -- "$root"

bash scripts/render-talos-storage-pool-layout.sh >"$scratch/rendered.yaml"
yq -o=json -I0 '.' ksail.prod.yaml >"$scratch/original.json"
yq -o=json -I0 'del(.spec.cluster.autoscaler.node.pools[].talosPatches)' "$scratch/rendered.yaml" >"$scratch/without-patches.json"
cmp "$scratch/original.json" "$scratch/without-patches.json"
yq -o=json -I0 '.spec.cluster.autoscaler.node.pools' "$scratch/rendered.yaml" |
  jq -e --rawfile rebuilt scripts/migrate-longhorn-v2/staging-layout.yaml \
    --rawfile retained scripts/migrate-longhorn-v2/retained-staging-layout.yaml '
    all(.[]; if .name == "autoscale-longhorn-v1" then .talosPatches == [$rebuilt]
      elif .name == "autoscale-longhorn-v2" then .talosPatches == [$retained]
      else has("talosPatches") | not end)
  ' >/dev/null

mkdir -p "$scratch/invalid/scripts/migrate-longhorn-v2"
cp scripts/migrate-longhorn-v2/{staging-layout.yaml,retained-staging-layout.yaml} "$scratch/invalid/scripts/migrate-longhorn-v2/"
yq '(.spec.cluster.autoscaler.node.pools[] | select(.name == "autoscale-longhorn-v2")).name = "autoscale-longhorn-v1"' \
  ksail.prod.yaml >"$scratch/invalid/ksail.prod.yaml"
if (cd -- "$scratch/invalid" && bash "$root/scripts/render-talos-storage-pool-layout.sh") >"$scratch/invalid.out" 2>/dev/null; then
  printf '%s\n' 'A duplicated migration pool unexpectedly rendered.' >&2
  exit 1
fi
test ! -s "$scratch/invalid.out"

cat >"$scratch/ksail" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ $# == 7 && "$1" == --config && -s "$2" && "$3 $4 $5 $6 $7" == "cluster update --dry-run --output json" ]]
[[ "$(yq '.spec.cluster.autoscaler.node.pools[] | select(.name == "autoscale-longhorn-v2") | .talosPatches | length' "$2")" == 1 ]]
cat -- "$KS_TEST_PLAN"
printf '%s\n' 'fixture diagnostics must stay private' >&2
EOF
chmod 0700 "$scratch/ksail"
cat >"$scratch/complete.json" <<'EOF'
{"totalChanges":1,"inPlaceChanges":[{"field":"private fixture field","category":"InPlace"}],"rebootRequired":[],"recreateRequired":[],"rollingRecreate":[],"wipeRequired":[],"unknownBaseline":[],"requiresConfirmation":false}
EOF
KS_TEST_PLAN="$scratch/complete.json" bash scripts/preview-prod-talos-pool-layout.sh "$scratch/ksail" \
  >"$scratch/summary.json" 2>"$scratch/summary.err"
test ! -s "$scratch/summary.err"
jq -e '. == {inPlace:1,reboot:0,recreate:0,rollingRecreate:0,wipe:0,unknown:0}' "$scratch/summary.json" >/dev/null

jq 'del(.unknownBaseline)' "$scratch/complete.json" >"$scratch/incomplete.json"
if KS_TEST_PLAN="$scratch/incomplete.json" bash scripts/preview-prod-talos-pool-layout.sh "$scratch/ksail" \
  >"$scratch/incomplete.out" 2>/dev/null; then
  printf '%s\n' 'An incomplete native update plan unexpectedly passed.' >&2
  exit 1
fi
test ! -s "$scratch/incomplete.out"
