# shellcheck shell=bash
# Freeze the legacy/dual-family test universe separately from canonical-only
# admission. The latter has its own real generator/render/writer controls.
build_legacy_consumer_fixture() {
  local legacy_fixture_repo="$1" legacy_fixture_dest="$2"
  [ ! -e "$legacy_fixture_dest" ] || return 1
  mkdir -p "$legacy_fixture_dest/scripts"
  cp -R "$legacy_fixture_repo/k8s" "$legacy_fixture_repo/talos" "$legacy_fixture_dest/"
  cp "$legacy_fixture_repo/ksail.prod.yaml" "$legacy_fixture_dest/ksail.prod.yaml"
  rm -rf "$legacy_fixture_dest/k8s/providers/hetzner/apps/business-site"
  yq -i '.resources |= map(select(. != "business-site/"))' "$legacy_fixture_dest/k8s/providers/hetzner/apps/kustomization.yaml"
  printf '{"schemaVersion":1,"bindings":[]}\n' >"$legacy_fixture_dest/scripts/canonical-tenant-source-bindings.json"
  awk -F '\t' '$1!="business-site"' "$legacy_fixture_repo/scripts/publish-workflow-canonical-approvals.tsv" >"$legacy_fixture_dest/scripts/publish-workflow-canonical-approvals.tsv"
}
