#!/usr/bin/env bash
# ResourceGraphDefinition workload templates are nested under a custom resource, so ordinary
# repository scans do not inspect them as Kubernetes manifests. This behavioural test proves the
# dedicated gate scans both the committed templates and a deliberately non-compliant mutation.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GATE="${REPO_ROOT}/scripts/scan-rgd-templates.sh"
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

# A future RGD must enter the scan automatically. Copying a valid current definition under a new
# sibling path reproduces the review finding: a fixed two-file list reports success while leaving
# this third definition completely unseen.
readonly PROBE_RGD="k8s/bases/infrastructure/resource-graph-definitions/probe/resource-graph-definition.yaml"
mkdir -p "$WORK/$(dirname "$PROBE_RGD")"
cp "$REPO_ROOT/$WEBAPP_RGD" "$WORK/$PROBE_RGD"
expect_rejected "an unbaselined third ResourceGraphDefinition" "$PROBE_RGD"
rm -f "$WORK/$PROBE_RGD"
rmdir "$WORK/$(dirname "$PROBE_RGD")"

# A baselined finding must retain its reviewed cause, not merely its ID and line range. Replacing
# the caller-supplied image expression with a forced mutable latest tag keeps KSV-0013 at the same
# location and count, so a count-only ratchet accepts the changed cause.
yq -i '(.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image) = "nginx:latest"' "$WORK/$WEBAPP_RGD"
[ "$(yq '.spec.resources[] | select(.id == "deployment") |
  .template.spec.template.spec.containers[0].image' "$WORK/$WEBAPP_RGD")" = "nginx:latest" ] ||
  fail "the baselined-cause regression fixture was not created"
expect_rejected "a changed value hidden behind the same finding ID and range" "TEMPLATE-CONTENT"
restore_webapp

# Committed RGD instances can feed template expressions values the extraction cannot infer. Guard
# the namespace-driving spec.name so an opt-in pilot cannot instantiate otherwise-safe templates
# inside kube-system while both the ordinary custom-resource scan and template scan stay green.
yq -i '.spec.name = "kube-system"' "$WORK/$WEBAPP_INSTANCE"
[ "$(yq '.spec.name' "$WORK/$WEBAPP_INSTANCE")" = "kube-system" ] ||
  fail "the unsafe committed-instance fixture was not created"
expect_rejected "a committed WebApp instance targeting kube-system" "KSV-0037"
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

echo "PASS: committed RGD templates pass; privilege, resource, and namespace regressions fail."
