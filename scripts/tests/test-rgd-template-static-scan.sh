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
readonly MAIN_WORKFLOW="${REPO_ROOT}/.github/workflows/validate-main.yaml"
readonly CI_WORKFLOW="${REPO_ROOT}/.github/workflows/ci.yaml"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ -x "$GATE" ] || fail "the RGD template static-scan gate is missing or not executable: $GATE"
command -v yq >/dev/null 2>&1 || fail "yq is required"
command -v trivy >/dev/null 2>&1 || fail "trivy is required"

# A direct push to main bypasses pull-request and merge-group validation. Keep the same behavioral
# gate on that path so the manual CD workflow cannot publish a nested-template regression unchecked.
main_workflow_json="$(yq -o=json -I=0 '.' "$MAIN_WORKFLOW")"
jq -e '
  any(.jobs["validate-rgd-templates"].steps[];
      (.uses // "") == "aquasecurity/setup-trivy@81e514348e19b6112ce2a7e3ecbafe19c1e1f567"
      and .with.version == "v0.74.0")
' <<<"$main_workflow_json" >/dev/null ||
  fail "validate-main.yaml does not pin the RGD gate's Trivy setup"
jq -e '
  any(.jobs["validate-rgd-templates"].steps[];
      (.run // "") | contains("bash scripts/tests/test-rgd-template-static-scan.sh"))
' <<<"$main_workflow_json" >/dev/null ||
  fail "validate-main.yaml does not run the RGD template behavioral gate"
ci_workflow_json="$(yq -o=json -I=0 '.' "$CI_WORKFLOW")"
jq -e '
  any(.jobs.changes.steps[];
      .id == "filter" and (.with.filters | contains(".trivy/data/**")))
' <<<"$ci_workflow_json" >/dev/null ||
  fail "ci.yaml does not run the RGD gate when its Trivy policy data changes"

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

# The content ratchet represents parsed template semantics, not yq's emitted whitespace. Put a
# wrapper first on PATH that adds a harmless blank line to YAML extraction; a byte-for-byte digest
# of serializer output fails here even though the parsed resources and Trivy findings are unchanged.
YQ_BIN="$(command -v yq)"
readonly YQ_BIN
mkdir -p "$WORK/bin"
cat >"$WORK/bin/yq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = '.spec.resources[].template | split_doc' ]; then
  "$REAL_YQ" "$@"
  printf '\n'
else
  exec "$REAL_YQ" "$@"
fi
EOF
chmod +x "$WORK/bin/yq"
PATH="$WORK/bin:$PATH" REAL_YQ="$YQ_BIN" "$GATE" "$WORK" ||
  fail "the content ratchet depends on yq's YAML serialization"
echo "PASS(probe): semantic content evidence ignores serializer-only whitespace."

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
