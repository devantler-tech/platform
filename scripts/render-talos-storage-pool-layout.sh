#!/usr/bin/env bash
# Pass through the production lifecycle config only when both migration pools
# still declare the layouts used by protected worker preparation.
set -euo pipefail

base=ksail.prod.yaml
rebuilt=scripts/migrate-longhorn-v2/staging-layout.yaml
retained=scripts/migrate-longhorn-v2/retained-staging-layout.yaml
if ! yq -o=json -I0 '.' "$base" | jq -es --rawfile rebuilt "$rebuilt" --rawfile retained "$retained" '
  def pool($name): [.spec.cluster.autoscaler.node.pools[] | select(.name == $name)];
  length == 1 and (.[0] |
    .metadata.name == "prod" and .spec.cluster.distribution == "Talos" and
    .spec.cluster.provider == "Hetzner" and
    (pool("autoscale-longhorn-v1") | map(.talosPatches) == [[$rebuilt]]) and
    (pool("autoscale-longhorn-v2") | map(.talosPatches) == [[$retained]]))
' >/dev/null; then
  printf '%s\n' 'Expected both reviewed disk profiles in the production Talos migration pools.' >&2
  exit 1
fi

cat -- "$base"
