#!/usr/bin/env bash
# Pin the path rewriting that scripts/normalize-sarif-paths.sh performs.
#
# WHY THIS EXISTS. That script is a fail-closed guard: it certifies that every
# path in a SARIF file resolves at the repository root before the file is
# uploaded to Code Scanning, because an unresolvable path produces an alert that
# is created, looks correct in the API, and is unclickable (#2830). Until this
# test existed the only gate over it was `shellcheck`, which cannot see a jq
# path that was never written — and that is exactly the gap #2863 found: the
# script rewrote `locations[]` while leaving `fixes[]` un-rooted, and its own
# closing re-check read only `locations[]`, so the guard stayed green while
# certifying four dead paths.
#
# The failure mode is therefore a MISSING rewrite, never a crash, which is why
# this asserts on the rewritten document rather than on exit status alone. Each
# assertion below is paired with a control where a control is what makes it
# meaningful.
#
# Bash plus the runner's jq; no cluster, no secrets, no network.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/normalize-sarif-paths.sh"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# A fixture repo root holding one real manifest under the k8s/ prefix, so the
# script's resolve-based rule has something true to resolve against.
readonly fixture_root="${work_dir}/repo"
mkdir -p "${fixture_root}/k8s/bases/apps/demo"
printf 'apiVersion: v1\nkind: ConfigMap\n' >"${fixture_root}/k8s/bases/apps/demo/cm.yaml"

# Emits a SARIF whose single result carries the same un-rooted path in BOTH
# locations[] and fixes[] — the shape Kubescape actually produces.
write_sarif() {
  local path="$1"
  cat >"${path}" <<'SARIF'
{
  "version": "2.1.0",
  "runs": [
    {
      "tool": { "driver": { "name": "kubescape" } },
      "results": [
        {
          "ruleId": "C-0034",
          "locations": [
            {
              "physicalLocation": {
                "artifactLocation": { "uri": "bases/apps/demo/cm.yaml" },
                "region": { "startLine": 1 }
              }
            }
          ],
          "fixes": [
            {
              "artifactChanges": [
                {
                  "artifactLocation": { "uri": "bases/apps/demo/cm.yaml" },
                  "replacements": [
                    { "deletedRegion": { "startLine": 1 } }
                  ]
                }
              ]
            }
          ]
        }
      ]
    }
  ]
}
SARIF
}

location_uris() {
  jq -r '[.runs[]?.results[]?.locations[]?.physicalLocation.artifactLocation.uri // empty] | unique[]' "$1"
}

fix_uris() {
  jq -r '[.runs[]?.results[]?.fixes[]?.artifactChanges[]?.artifactLocation.uri // empty] | unique[]' "$1"
}

# Every path the document still carries must resolve under the fixture root.
# This is the property the script promises; it is asserted over BOTH fields
# because a promise that silently excludes one of them is the defect itself.
assert_all_resolve() {
  local sarif="$1"
  local label="$2"
  local uri

  while IFS= read -r uri; do
    [ -n "${uri}" ] || continue
    [ -e "${fixture_root}/${uri}" ] ||
      fail "${label}: path does not resolve under the repo root: ${uri}"
  done < <(
    location_uris "${sarif}"
    fix_uris "${sarif}"
  )
}

# ---------------------------------------------------------------------------
# 1. Both fields are re-rooted.
# ---------------------------------------------------------------------------
sarif="${work_dir}/both.sarif"
write_sarif "${sarif}"

# CONTROL: before the script runs, neither field resolves. Without this the
# assertion below would also pass against a script that did nothing at all on an
# input that happened to be correct already.
[ ! -e "${fixture_root}/$(location_uris "${sarif}")" ] ||
  fail "control: the fixture location path already resolved, so the rewrite is untestable"
[ ! -e "${fixture_root}/$(fix_uris "${sarif}")" ] ||
  fail "control: the fixture fixes path already resolved, so the rewrite is untestable"

bash "${script}" "${sarif}" k8s "${fixture_root}" >/dev/null

[ "$(location_uris "${sarif}")" = "k8s/bases/apps/demo/cm.yaml" ] ||
  fail "locations[] was not re-rooted (got: $(location_uris "${sarif}"))"

# THE REGRESSION THIS FILE EXISTS FOR (#2863). Kubescape repeats the path under
# fixes[]; leaving it un-rooted keeps a dead path in a document the script has
# just certified as clean.
[ "$(fix_uris "${sarif}")" = "k8s/bases/apps/demo/cm.yaml" ] ||
  fail "fixes[].artifactChanges[].artifactLocation.uri was not re-rooted (got: $(fix_uris "${sarif}"))"

assert_all_resolve "${sarif}" "after rewrite"

# ---------------------------------------------------------------------------
# 2. Idempotent: a second run over an already-rooted document changes nothing.
# ---------------------------------------------------------------------------
before="$(cat "${sarif}")"
bash "${script}" "${sarif}" k8s "${fixture_root}" >/dev/null
[ "${before}" = "$(cat "${sarif}")" ] ||
  fail "second run mutated an already-root-relative document; the rewrite is not idempotent"

# ---------------------------------------------------------------------------
# 3. A path that resolves under NEITHER root is a hard error, not a silent pass.
# ---------------------------------------------------------------------------
bad_sarif="${work_dir}/bad.sarif"
write_sarif "${bad_sarif}"
jq '.runs[0].results[0].locations[0].physicalLocation.artifactLocation.uri = "bases/apps/ghost/nope.yaml"' \
  "${bad_sarif}" >"${bad_sarif}.tmp" && mv "${bad_sarif}.tmp" "${bad_sarif}"

if bash "${script}" "${bad_sarif}" k8s "${fixture_root}" >/dev/null 2>&1; then
  fail "an unresolvable path was accepted; the fail-closed guard did not fire"
fi

# ---------------------------------------------------------------------------
# 4. An empty uri is dropped with a warning, not treated as an error.
#    Kubescape emits one for cluster-RBAC controls with no backing manifest.
# ---------------------------------------------------------------------------
empty_sarif="${work_dir}/empty.sarif"
write_sarif "${empty_sarif}"
jq '.runs[0].results[0].locations[0].physicalLocation.artifactLocation.uri = ""' \
  "${empty_sarif}" >"${empty_sarif}.tmp" && mv "${empty_sarif}.tmp" "${empty_sarif}"

bash "${script}" "${empty_sarif}" k8s "${fixture_root}" >/dev/null ||
  fail "an empty uri should be dropped with a warning, not fail the run"
[ "$(jq '[.runs[]?.results[]?] | length' "${empty_sarif}")" = "0" ] ||
  fail "the empty-uri result was not dropped"

# ---------------------------------------------------------------------------
# 5. A clean scan (zero results) is not an error.
# ---------------------------------------------------------------------------
clean_sarif="${work_dir}/clean.sarif"
printf '%s\n' '{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"kubescape"}},"results":[]}]}' \
  >"${clean_sarif}"
bash "${script}" "${clean_sarif}" k8s "${fixture_root}" >/dev/null ||
  fail "a zero-result SARIF should succeed; the clean-scan case must not fail closed"

# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# 6. An artifactChange that identifies its artifact by INDEX has no uri, and
#    must not acquire one.
#
#    SARIF lets an artifactLocation reference the run's artifacts[] table by
#    index instead of carrying a uri. jq's `|=` visits an absent path and writes
#    the result back, so rewriting `.artifactLocation.uri` on such an entry
#    CREATES `"uri": null`. The schema requires uri to be a string when present,
#    so the document becomes invalid and Code Scanning can reject the whole
#    upload — turning a path fix into a total findings outage.
#
#    Both shapes sit in one artifactChanges[] so the control is exact: the
#    uri-identified entry must still be re-rooted in the same pass that leaves
#    the index-identified one alone. Skipping fixes[] wholesale would satisfy
#    the first assertion and reintroduce the very bug #2863 fixed.
# ---------------------------------------------------------------------------
index_sarif="${work_dir}/index.sarif"
write_sarif "${index_sarif}"
jq '.runs[0].results[0].fixes[0].artifactChanges += [{"artifactLocation": {"index": 0}}]' \
  "${index_sarif}" >"${index_sarif}.tmp" && mv "${index_sarif}.tmp" "${index_sarif}"

bash "${script}" "${index_sarif}" k8s "${fixture_root}" >/dev/null ||
  fail "an index-identified artifactChange should not fail the run"

jq -e '.runs[0].results[0].fixes[0].artifactChanges[1].artifactLocation | has("uri") | not' \
  "${index_sarif}" >/dev/null ||
  fail "rewriting an index-identified artifactLocation created a uri key; SARIF requires uri to be a string when present, so the document is now schema-invalid"

jq -e '.runs[0].results[0].fixes[0].artifactChanges[1].artifactLocation.index == 0' \
  "${index_sarif}" >/dev/null ||
  fail "the index reference was lost"

[ "$(jq -r '.runs[0].results[0].fixes[0].artifactChanges[0].artifactLocation.uri' "${index_sarif}")" \
  = "k8s/bases/apps/demo/cm.yaml" ] ||
  fail "the uri-identified artifactChange beside it was left un-rooted; the index guard is too broad"
# 7. CI actually runs this test, so the gate cannot be added and then orphaned.
# ---------------------------------------------------------------------------
grep -Fq 'scripts/tests/test-normalize-sarif-paths.sh' "${ci_workflow}" ||
  fail "ci.yaml does not reference this test; adding it without wiring it in gates nothing"

printf 'TEST PASS: normalize-sarif-paths rewrites locations[] and fixes[], stays idempotent, and fails closed.\n'
