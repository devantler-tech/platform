# shellcheck shell=bash
# Freeze the legacy/dual-family consumer test universe: the repository's manifests and
# Talos configuration with its reviewed canonical approvals. The Tenant graph is no
# shared-publisher consumer (ADR 0019), so its instances need no separate admission.
build_legacy_consumer_fixture() {
  local legacy_fixture_repo="$1" legacy_fixture_dest="$2"
  [ ! -e "$legacy_fixture_dest" ] || return 1
  mkdir -p "$legacy_fixture_dest/scripts"
  cp -R "$legacy_fixture_repo/k8s" "$legacy_fixture_repo/talos" "$legacy_fixture_dest/"
  cp "$legacy_fixture_repo/ksail.prod.yaml" "$legacy_fixture_dest/ksail.prod.yaml"
  cp "$legacy_fixture_repo/scripts/publish-workflow-canonical-approvals.tsv" "$legacy_fixture_dest/scripts/publish-workflow-canonical-approvals.tsv"
}
