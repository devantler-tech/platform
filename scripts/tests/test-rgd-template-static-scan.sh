#!/usr/bin/env bash
# ResourceGraphDefinition workload templates are nested under a custom resource, so ordinary
# repository scans do not inspect them as Kubernetes manifests. This behavioural test proves the
# dedicated gate scans both the committed templates and a deliberately non-compliant mutation.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GATE="${REPO_ROOT}/scripts/scan-rgd-templates.sh"
readonly BASELINE="${REPO_ROOT}/scripts/rgd-template-static-scan-baseline.tsv"
readonly WIRING_TEST="${REPO_ROOT}/scripts/tests/test-rgd-template-static-scan-wiring.sh"
readonly WEBAPP_RGD="k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"
readonly TENANT_RGD="k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"
readonly WEBAPP_INSTANCE="k8s/providers/docker/apps/web-app-wedding-app.yaml"
readonly TENANT_INSTANCE="k8s/providers/docker/apps/tenant-ascoachingogvaner.yaml"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ -x "$GATE" ] || fail "the RGD template static-scan gate is missing or not executable: $GATE"
command -v yq >/dev/null 2>&1 || fail "yq is required"
command -v trivy >/dev/null 2>&1 || fail "trivy is required"

# shellcheck disable=SC2016 # These are literal source invariants, not test-shell arithmetic.
grep -Fq 'semantic_start_line=$((finding_start_line - leading_blank_lines - 1))' "$GATE" ||
  fail "Trivy one-based start lines are not converted to yq zero-based lines"
# shellcheck disable=SC2016 # These are literal source invariants, not test-shell arithmetic.
grep -Fq 'semantic_end_line=$((finding_end_line - leading_blank_lines - 1))' "$GATE" ||
  fail "Trivy one-based end lines are not converted to yq zero-based lines"

"$WIRING_TEST" || fail "the RGD gate is not wired on every protected event route"

# The real committed templates are the positive control. A gate that rejects them cannot be wired
# into pull requests, and a test that exercises only the mutation cannot prove that it can.
"$GATE" "$REPO_ROOT" || fail "the committed RGD templates do not pass the static-scan gate"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$(dirname "$WEBAPP_RGD")" "$WORK/$(dirname "$TENANT_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$WEBAPP_RGD"
cp "$REPO_ROOT/$TENANT_RGD" "$WORK/$TENANT_RGD"
mkdir -p "$WORK/$(dirname "$WEBAPP_INSTANCE")"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"
cp "$REPO_ROOT/$TENANT_INSTANCE" "$WORK/$TENANT_INSTANCE"

# Kind alone does not identify a generated KRO API. An unrelated group may legitimately define the
# same Kind; it must not inherit KRO-specific placement or substitution policies.
readonly UNRELATED_INSTANCE="k8s/providers/docker/apps/unrelated-webapp.yaml"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$UNRELATED_INSTANCE"
yq -i '.apiVersion = "example.io/v1" | .spec.name = "kube-system"' "$WORK/$UNRELATED_INSTANCE"
"$GATE" "$WORK" || fail "an unrelated API group with a shared Kind was treated as an RGD instance"
rm -f "$WORK/$UNRELATED_INSTANCE"
echo "PASS(probe): RGD instances are matched by API version and kind."

# Finding counts must remain attributable to individual nested resources. Otherwise removing one
# KSV-0039 finding while introducing the same rule on a sibling resource leaves the aggregate row
# unchanged after the graph digest is reviewed.
tenant_ksv0039_targets="$({
  awk -F '\t' '$2 ~ /resource-graph-definitions\/tenant/ && $3 == "KSV-0039" { print $2 }' "$BASELINE" |
    LC_ALL=C sort -u
} | wc -l | tr -d ' ')"
[ "$tenant_ksv0039_targets" -gt 1 ] ||
  fail "the finding ratchet collapses KSV-0039 across Tenant resources"

# The content ratchet represents parsed template semantics, not yq's emitted presentation. Put a
# wrapper first on PATH that adds harmless blank lines and double-quotes every string in each
# serialized resource. This changes Trivy's ranges and cause text without changing the parsed
# manifest, so finding identity must remain stable.
YQ_BIN="$(command -v yq)"
readonly YQ_BIN
mkdir -p "$WORK/bin"
cat >"$WORK/bin/yq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = '(.spec.resources[] | select(.id == strenv(RESOURCE_ID))).template' ]; then
  printf '\n\n'
  "$REAL_YQ" "$@" | "$REAL_YQ" '(.. | select(tag == "!!str")) style="double"'
else
  exec "$REAL_YQ" "$@"
fi
EOF
chmod +x "$WORK/bin/yq"
PATH="$WORK/bin:$PATH" REAL_YQ="$YQ_BIN" "$GATE" "$WORK" ||
  fail "the finding or content ratchet depends on yq's YAML serialization"
echo "PASS(probe): finding and content evidence ignore serializer-only presentation."

expect_rejected() {
  local label="$1" expected_id="$2" log="${WORK}/gate-probe.log"
  if "$GATE" "$WORK" >"$log" 2>&1; then
    fail "the gate accepted $label"
  fi
  if ! grep -Fq "$expected_id" "$log"; then
    sed 's/^/  /' "$log" >&2
    fail "the gate failed for $label without reporting the expected $expected_id finding"
  fi
  echo "PASS(probe): $label is rejected with $expected_id."
}

restore_webapp() {
  cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$WEBAPP_RGD"
}

restore_tenant() {
  cp "$REPO_ROOT/$TENANT_RGD" "$WORK/$TENANT_RGD"
}

# A future RGD must enter the scan automatically. Copying a valid current definition under a new
# sibling path reproduces the review finding: a fixed two-file list reports success while leaving
# this third definition completely unseen.
readonly PROBE_RGD="k8s/bases/infrastructure/resource-graph-definitions/probe/resource-graph-definition.yaml"
mkdir -p "$WORK/$(dirname "$PROBE_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROBE_RGD"
expect_rejected "an unbaselined third ResourceGraphDefinition" "$PROBE_RGD"
rm -f "$WORK/$PROBE_RGD"
rmdir "$WORK/$(dirname "$PROBE_RGD")"

# Version-only schema APIs use the generated group encoded after the first dot in metadata.name,
# not the outer ResourceGraphDefinition CRD group. A custom-group instance must therefore still
# enter the committed-instance placement and substitution guards.
readonly GROUPED_INSTANCE="k8s/providers/docker/apps/grouped-webapp-probe.yaml"
mkdir -p "$WORK/$(dirname "$PROBE_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROBE_RGD"
yq -i '.metadata.name = "groupedwebapp.platform.example" |
  .spec.schema.kind = "GroupedWebApp"' "$WORK/$PROBE_RGD"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$GROUPED_INSTANCE"
yq -i '.apiVersion = "platform.example/v1alpha1" |
  .kind = "GroupedWebApp" | .spec.name = "kube-system"' "$WORK/$GROUPED_INSTANCE"
expect_rejected "a custom-group RGD instance targeting kube-system" "KSV-0037"
rm -f "$WORK/$GROUPED_INSTANCE" "$WORK/$PROBE_RGD"
rmdir "$WORK/$(dirname "$PROBE_RGD")"

# Provider overlays may consume the base RGDs, but they may not mutate a graph after the base-only
# extraction. A targeted JSON patch would otherwise make the rendered provider diverge invisibly.
readonly PROVIDER_PROBE="k8s/providers/probe/infrastructure/kustomization.yaml"
mkdir -p "$WORK/$(dirname "$PROVIDER_PROBE")"
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "kind": "ResourceGraphDefinition",
      "name": "webapp.kro.run"
    },
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a provider overlay patch targeting an RGD" "ResourceGraphDefinition"
rm -f "$WORK/$PROVIDER_PROBE"

# A Kustomize selector may omit kind and still match every resource satisfying its remaining
# fields. Such a selector must not bypass the provider-overlay guard merely because `.target.kind`
# is absent.
yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" | .resources = [] | .patches = [{
    "target": {
      "group": "kro.run",
      "version": "v1alpha1",
      "name": "webapp.kro.run"
    },
    "patch": "- op: add\n  path: /spec/resources/0/template/spec/privileged\n  value: true"
  }]' >"$WORK/$PROVIDER_PROBE"
expect_rejected "a provider overlay RGD selector without kind" "ResourceGraphDefinition"
rm -f "$WORK/$PROVIDER_PROBE"

# Provider-local definitions are valid graph sources rather than mutations, but they must enter the
# same evidence baseline. Leaving discovery rooted only in the shared base would certify neither
# their nested resources nor their graph content.
readonly PROVIDER_RGD="k8s/providers/probe/infrastructure/provider-resource-graph-definition.yaml"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROVIDER_RGD"
yq -i '.metadata.name = "providerwebapp.kro.run" |
  .spec.schema.kind = "ProviderWebApp"' "$WORK/$PROVIDER_RGD"
expect_rejected "an unbaselined provider ResourceGraphDefinition" "$PROVIDER_RGD"
rm -f "$WORK/$PROVIDER_RGD"
rmdir "$WORK/$(dirname "$PROVIDER_PROBE")"

# Resource-level graph controls decide whether a template is instantiated. They must be evidence too:
# gating the default-deny NetworkPolicy changes tenant isolation without changing its template bytes.
# shellcheck disable=SC2016 # the KRO expression must remain literal in the fixture
yq -i '(.spec.resources[] | select(.id == "defaultDeny").includeWhen) =
  ["${schema.spec.resourceQuota.enabled}"]' "$WORK/$TENANT_RGD"
expect_rejected "a condition that disables default-deny for defaulted tenants" "RGD-CONTENT"
restore_tenant

# A baselined finding must retain its reviewed cause, not merely its ID and line range. Replacing
# the caller-supplied image expression with a forced mutable latest tag keeps KSV-0013 at the same
# location and count, so a count-only ratchet accepts the changed cause.
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image) = "nginx:latest"' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image' "$WORK/$WEBAPP_RGD")" = "nginx:latest" ] ||
  fail "the baselined-cause regression fixture was not created"
expect_rejected "a changed value hidden behind the same finding ID and range" "RGD-CONTENT"
restore_webapp

# Moving an existing rule between containers must be visible even when its resource-level count and
# Trivy cause text are unchanged. Clone the reviewed container byte-for-byte, then pin only the
# original image: KSV-0013 moves from containers[0] to containers[1] with identical cause content.
# The graph digest changes too, but the finding diff must retain both old and new semantic paths.
# shellcheck disable=SC2016 # $reviewed_container is a yq variable, not a shell expansion
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0]) as $reviewed_container |
  (.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image) =
  "ghcr.io/example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" |
  (.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers) += [$reviewed_container]' "$WORK/$WEBAPP_RGD"
container_identity_log="${WORK}/container-identity.log"
if "$GATE" "$WORK" >"$container_identity_log" 2>&1; then
  fail "the gate accepted a KSV-0013 move between workload containers"
fi
container_cause_count="$(awk -F '\t' '
  $3 == "KSV-0013" && index($5, "CAUSE-SHA256:") == 1 { print $5 }
' "$container_identity_log" | LC_ALL=C sort -u | wc -l | tr -d ' ')"
[ "$container_cause_count" -gt 1 ] || {
  sed 's/^/  /' "$container_identity_log" >&2
  fail "the finding ratchet hides a KSV-0013 move between workload containers"
}
echo "PASS(probe): container-level finding identity cannot cancel within one resource."
restore_webapp

# Committed RGD instances can feed template expressions values the extraction cannot infer. Guard
# the namespace-driving spec.name so an opt-in pilot cannot instantiate otherwise-safe templates
# inside kube-system while both the ordinary custom-resource scan and template scan stay green.
yq -i '.spec.name = "kube-system"' "$WORK/$WEBAPP_INSTANCE"
[ "$(yq '.spec.name' "$WORK/$WEBAPP_INSTANCE")" = "kube-system" ] ||
  fail "the unsafe committed-instance fixture was not created"
expect_rejected "a committed WebApp instance targeting kube-system" "KSV-0037"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Parse the kind as YAML rather than prefiltering its serialized spelling. Quoting is valid and must
# not let the same unsafe instance bypass the placement guard.
yq -i '.kind style="double" | .spec.name = "kube-system"' "$WORK/$WEBAPP_INSTANCE"
[ "$(yq '.kind' "$WORK/$WEBAPP_INSTANCE")" = "WebApp" ] ||
  fail "the quoted-kind committed-instance fixture was not created"
expect_rejected "a quoted-kind WebApp instance targeting kube-system" "KSV-0037"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Security-sensitive substituted values need their own policy checks. The outer WebApp CR is not a
# workload Trivy recognizes, so an untrusted image registry otherwise remains invisible.
yq -i '.spec.image = "evil.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
  "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a committed WebApp image from an untrusted registry" "KSV-0125"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# A hostname that merely ends with a trusted registry is still a different registry. The RGD
# instance guard must be stricter than Trivy's suffix-based KSV-0125 implementation here.
yq -i '.spec.image = "attackerghcr.io/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
  "$WORK/$WEBAPP_INSTANCE"
expect_rejected "a look-alike committed WebApp image registry" "KSV-0125"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Ratchet the complete parsed instance as well as explicit security policies. That keeps every value
# substituted into a generated resource review-visible, including fields without a Trivy rule today.
yq -i '.spec.host = "unexpected.example"' "$WORK/$WEBAPP_INSTANCE"
expect_rejected "an unreviewed committed instance substitution" "INSTANCE-CONTENT"
cp "$REPO_ROOT/$WEBAPP_INSTANCE" "$WORK/$WEBAPP_INSTANCE"

# Mutate the nested Deployment rather than adding a top-level manifest. `privileged: true` is a
# scanner-supported Kubernetes misconfiguration and represents exactly the blind spot this gate
# closes: the outer RGD remains valid while its generated workload becomes unsafe.
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].securityContext.privileged) = true' \
  "$WORK/$WEBAPP_RGD"

[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].securityContext.privileged' "$WORK/$WEBAPP_RGD")" = "true" ] ||
  fail "the non-compliant nested-template fixture was not created"

expect_rejected "a privileged container" "KSV-0017"

# The other concrete failure classes from #3055 stay covered too. Each mutation starts from the
# committed RGD so one finding cannot make another assertion pass accidentally.
restore_webapp
yq -i 'del(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].resources)' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0] | has("resources")' "$WORK/$WEBAPP_RGD")" = "false" ] ||
  fail "the missing-resource-limits fixture was not created"
expect_rejected "a container without resource requests or limits" "KSV-0011"

restore_webapp
yq -i '(.spec.resources[] | select(.id == "deployment") | .template.metadata.namespace) =
  "kube-system"' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.metadata.namespace' "$WORK/$WEBAPP_RGD")" = "kube-system" ] ||
  fail "the kube-system placement fixture was not created"
expect_rejected "a tenant workload placed in kube-system" "KSV-0037"

echo "PASS: committed RGD graphs and instances pass; graph, substitution, and workload regressions fail."
