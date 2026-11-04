#!/usr/bin/env bash
# A ValidatingPolicy reports one result per resource, so a kyverno test row cannot
# say which check refused a resource, and most fixtures fail more than one check.
# This runs each check alone against the rows recorded for it in per-check.yaml, and
# requires the rows to go red when that check accepts everything.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-github-team-management.yaml"
tests_dir="${repo_root}/tests/restrict-github-team-management"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# In the order of spec.validations.
checks=(
  teams-allow-listed
  teams-bind-provider-identity-to-object-name
  teams-not-nested-or-directory-linked
  teammemberships-reference-allow-listed-teams
  teammemberships-allow-listed-members
  teammemberships-known-roles
  teamrepositories-reference-allow-listed-teams
  teamrepositories-maintainers-capped-below-admin
)

count="$(yq '.spec.validations | length' "${policy}")"
if [ "${count}" != "${#checks[@]}" ]; then
  echo "the policy has ${count} checks but this test names ${#checks[@]}" >&2
  exit 1
fi

rows_total=0
for index in "${!checks[@]}"; do
  check="${checks[${index}]}"
  fail_rows=0
  rows=0
  for rows_file in "${tests_dir}"/per-check.yaml "${tests_dir}"/*/per-check.yaml; do
    fixture="$(dirname "${rows_file}")"
    case_dir="${work}/${index}-$(basename "${fixture}")"
    mkdir -p "${case_dir}/kept" "${case_dir}/weakened"

    CHECK="${check}" yq -o=json '.results | map(select(.rule == strenv(CHECK)))' "${rows_file}" \
      >"${case_dir}/rows.json"
    [ "$(jq 'length' "${case_dir}/rows.json")" -gt 0 ] || continue
    rows=$((rows + $(jq '[.[].resources[]] | length' "${case_dir}/rows.json")))
    fail_rows=$((fail_rows + $(jq '[.[] | select(.result == "fail") | .resources[]] | length' "${case_dir}/rows.json")))

    for variant in kept weakened; do
      target="${case_dir}/${variant}"
      cp "${fixture}/resources.yaml" "${target}/resources.yaml"
      if [ "${variant}" = kept ]; then
        INDEX="${index}" yq '.spec.validations = [.spec.validations[env(INDEX)]]' "${policy}" >"${target}/policy.yaml"
      else
        INDEX="${index}" yq '.spec.validations = [.spec.validations[env(INDEX)] | .expression = "true"]' \
          "${policy}" >"${target}/policy.yaml"
      fi
      # A check that does not apply to a resource accepts it, where the classic
      # rule's false precondition reported a skip.
      jq '{apiVersion: "cli.kyverno.io/v1alpha1", kind: "Test", metadata: {name: "check"},
           policies: ["policy.yaml"], resources: ["resources.yaml"],
           results: map(del(.rule) | .result |= (if . == "skip" then "pass" else . end))}' \
        "${case_dir}/rows.json" >"${target}/kyverno-test.yaml"
    done

    if ! kyverno test "${case_dir}/kept" --remove-color >"${case_dir}/kept.log" 2>&1; then
      cat "${case_dir}/kept.log"
      echo "${check}: its rows in ${rows_file} do not hold when it runs alone" >&2
      exit 1
    fi
    if [ "$(jq '[.[] | select(.result == "fail")] | length' "${case_dir}/rows.json")" -gt 0 ] &&
      kyverno test "${case_dir}/weakened" --remove-color >"${case_dir}/weakened.log" 2>&1; then
      echo "${check}: its fail rows in ${rows_file} still hold with the check removed" >&2
      exit 1
    fi
  done
  if [ "${fail_rows}" -eq 0 ]; then
    echo "${check}: no fixture proves this check refuses anything" >&2
    exit 1
  fi
  rows_total=$((rows_total + rows))
  echo "ok: ${check} (${rows} rows, ${fail_rows} refused)"
done
echo "All ${#checks[@]} checks hold their ${rows_total} recorded rows alone."
