#!/usr/bin/env bash
# Keep the two reviewed migration layouts in Talos lifecycle configuration.
set -euo pipefail

base=ksail.prod.yaml
rebuilt=scripts/migrate-longhorn-v2/staging-layout.yaml
retained=scripts/migrate-longhorn-v2/retained-staging-layout.yaml
if ! yq -o=json -I0 '.' "$base" | jq -es '
  def pool($name): [.spec.cluster.autoscaler.node.pools[] | select(.name == $name)];
  length == 1 and (.[0] |
    .metadata.name == "prod" and .spec.cluster.distribution == "Talos" and
    .spec.cluster.provider == "Hetzner" and
    (pool("autoscale-longhorn-v1") | length == 1 and all(.[]; has("talosPatches") | not)) and
    (pool("autoscale-longhorn-v2") | length == 1 and all(.[]; has("talosPatches") | not)))
' >/dev/null; then
  printf '%s\n' 'Expected two unique unmodified Longhorn migration pools on Talos production.' >&2
  exit 1
fi

STAGING_REBUILT_LAYOUT="$rebuilt" STAGING_RETAINED_LAYOUT="$retained" yq eval '
  (.spec.cluster.autoscaler.node.pools[] | select(.name == "autoscale-longhorn-v1") | .talosPatches) =
    [load_str(strenv(STAGING_REBUILT_LAYOUT))] |
  (.spec.cluster.autoscaler.node.pools[] | select(.name == "autoscale-longhorn-v2") | .talosPatches) =
    [load_str(strenv(STAGING_RETAINED_LAYOUT))]
' "$base"
