#!/usr/bin/env bash
# Exercise the actual OCI and publisher guards against the complete Tenant graph.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
graph=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
failures=0
new_case() {
  case_root="$work/$1"
  mkdir -p "$case_root/$(dirname "$graph")" "$case_root/scripts"
  cp "$root/$graph" "$case_root/$graph"
  cp "$root/scripts/guard-shared-publish-workflow-pin.sh" "$case_root/scripts/"
  if [[ -f "$root/scripts/tenant-source-constructors.lib.sh" ]]; then
    cp "$root/scripts/tenant-source-constructors.lib.sh" "$case_root/scripts/"
  fi
  printf '1\t%s\n' "$graph" >"$case_root/scripts/shared-publish-workflow-matchers.tsv"
  printf 'resources:\n  - resource-graph-definition.yaml\n' >"$case_root/$(dirname "$graph")/kustomization.yaml"
  : >"$case_root/exemptions.tsv"
}
check_guards() {
  local label="$1" want="$2" guard out rc
  for guard in oci publisher; do
    rc=0
    if [[ "$guard" == oci ]]; then
      out="$(OCI_VERIFY_ROOTS="$case_root/$(dirname "$graph")" OCI_VERIFY_EXEMPTIONS_FILE="$case_root/exemptions.tsv" OCI_VERIFY_EXPECTED_MIN=1 \
        bash "$root/scripts/guard-oci-repository-verify.sh" 2>&1)" || rc=$?
    else
      out="$(bash "$case_root/scripts/guard-shared-publish-workflow-pin.sh" 2>&1)" || rc=$?
    fi
    if [[ "$want" == accept && "$rc" == 0 ]]; then
      printf 'ok: %s %s accepted\n' "$guard" "$label"
    elif [[ "$want" == reject && "$rc" != 0 && "$out" == *'invalid Tenant source constructor'* ]]; then
      printf 'ok: %s %s refused by constructor boundary\n' "$guard" "$label"
    elif [[ "$want" == reject && "$guard" == oci && ( "$label" == foreign-source-url || "$label" == wrong-node-kind ) && "$rc" != 0 && "$out" == *'found 0 in-scope OCIRepositor(y/ies)'* ]]; then
      # These mutations remove the only kind/URL in this isolated discovery root.
      # The publisher guard independently rejects their complete graph context.
      printf 'ok: oci %s refused by the discovery floor\n' "$label"
    else
      printf 'FAIL: %s %s expected %s, exit=%s: %s\n' "$guard" "$label" "$want" "$rc" "$out" >&2
      failures=$((failures + 1))
    fi
  done
}
new_case committed
check_guards 'complete constrained graph' accept

# A plausible complete document followed by a failed reader is not evidence.
# Invoke the library without caller pipefail so its own failure boundary is tested.
mkdir -p "$work/partial-reader"
real_yq="$(command -v yq)"
printf '#!/usr/bin/env bash\n%q "$@"\nexit 23\n' "$real_yq" >"$work/partial-reader/yq"
chmod +x "$work/partial-reader/yq"
if PATH="$work/partial-reader:$PATH" bash -c 'source "$1"; tenant_source_default_subject "$2"' \
  _ "$root/scripts/tenant-source-constructors.lib.sh" "$case_root/$graph" >"$work/partial.log" 2>&1; then
  printf 'FAIL: partial reader failure was accepted\n' >&2
  failures=$((failures + 1))
elif grep -q 'invalid Tenant source constructor' "$work/partial.log"; then
  printf 'ok: partial reader failure refused independently of caller pipefail\n'
else
  printf 'FAIL: partial reader refusal did not name the constructor boundary\n' >&2
  failures=$((failures + 1))
fi
while IFS=$'\t' read -r label expression; do
  [[ -n "$label" ]] || continue
  new_case "$label"
  yq -i "$expression" "$case_root/$graph"
  check_guards "$label" reject
done <<'CASES'
wrong-graph-name	.metadata.name = "other.kro.run"
wrong-generated-kind	.spec.schema.kind = "Other"
wrong-scope	.spec.schema.scope = "Cluster"
unbounded-digest	.spec.schema.types.TenantSource.digest = "string"
unbounded-publisher	.spec.schema.types.TenantSource.publisherRevision = "string"
missing-pair-rule	.spec.schema.spec.source = "TenantSource | default={}"
unsafe-active-toggle	.spec.schema.types.TenantSource.reconcileApplication = "boolean | default=true"
extra-source-field	.spec.schema.types.TenantSource.url = "string"
wrong-child-id	(.spec.resources[] | select(.id == "ociRepository").id) = "otherSource"
foreign-source-url	(.spec.resources[] | select(.id == "ociRepository").template.spec.url) = "oci://example.invalid/other"
foreign-namespace	(.spec.resources[] | select(.id == "ociRepository").template.metadata.namespace) = "other"
tag-constructor	(.spec.resources[] | select(.id == "ociRepository").template.spec.ref) = "${schema.spec.source.digest}"
wildcard-subject	(.spec.resources[] | select(.id == "ociRepository").template.spec.verify.matchOIDCIdentity[0].subject) += "|.*"
second-identity	(.spec.resources[] | select(.id == "ociRepository").template.spec.verify.matchOIDCIdentity) += [{"issuer":".*","subject":".*"}]
wrong-node-kind	(.spec.resources[] | select(.id == "ociRepository").template.kind) = "Other"
CASES
printf '%s failure(s)\n' "$failures"
[[ "$failures" == 0 ]]
