#!/usr/bin/env bash
# Ask the production API server whether restrict-github-team-external-identity really refuses a
# foreign GitHub team identity, and really admits the legitimate writes (#3144).
#
# THE QUESTION. The policy pins which GitHub team a github-config Team object reconciles. Its
# fixtures pass and the policy reads Ready, but neither shows that production admission denies a
# foreign identity, or that the provider's own write still gets through. This script obtains
# exactly those observations.
#
# WHAT IT DOES. It persists nothing: every write it sends carries `--dry-run=server`, so the API
# server runs the request through admission and then discards it.
#   1. `get clusterpolicy`          — the policy must be Ready and set to Enforce.
#   2. `get teams` in github-config — the Team objects and the identity each one carries.
#   3. `get pods` in crossplane-system — the ServiceAccount the GitHub provider runs as.
#   4. For every Team, as the github-config applier (impersonated):
#        unchanged   patch the identity to the value it already has      → must be admitted
#        foreign     patch the identity to one that is not approved      → must be refused
#        swapped     patch the identity to another Team's identity       → must be refused
#        re-create   create the object with a foreign identity           → must be refused
#        adoption    create the object with the identity it carries      → must pass admission
#      and, as the provider:
#        provider    patch the identity to one that is not approved      → must be admitted
#
# HOW AN ANSWER IS READ. A refusal counts only when the API server names THIS policy in it. A
# Forbidden from RBAC, a refusal from another policy or a failed connection would all look like
# "refused" otherwise, and the probe would report enforcement it never observed.
# "adoption" re-creates an object that exists, so it can never be stored: the API server runs
# admission first and answers AlreadyExists only once admission has let the request through. That
# answer is therefore the admitted one for that probe, and a refusal naming this policy is not.
#
# THE VERDICT
#   ENFORCED       every probe above gave the expected answer.
#   NOT-ENFORCED   a foreign identity was admitted, or this policy refused a legitimate write.
#   INCONCLUSIVE   anything else: a failed read, the policy not Ready or not enforcing, no Team to
#                  probe, an identity that is not a number, no single provider ServiceAccount, or
#                  an answer that could not be attributed to this policy.
#
# WHAT IT PRINTS. The workflow log of a public repository is public. Only Team names, probe names
# and outcomes are printed: no identity, no ServiceAccount name, and never kubectl's own error
# text, which can carry the API server address.
#
# EXIT CODES
#   0  ENFORCED
#   1  usage error — nothing was read
#   2  NOT-ENFORCED
#   3  INCONCLUSIVE — the probe proved nothing, and a caller must not report it as green
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly policy='restrict-github-team-external-identity'
readonly team_resource='teams.team.github.m.upbound.io'
readonly team_namespace='github-config'
readonly applier='system:serviceaccount:github-config:github-config'
readonly provider_namespace='crossplane-system'
readonly provider_prefix='provider-upjet-github-'
readonly identity_annotation='crossplane.io/external-name'
# Not an approved identity of any Team, and GitHub does not hand it out again.
readonly foreign_identity='1'

usage() {
  printf 'Usage: %s --context <kube-context>\n' "$(basename "$0")" >&2
}

context=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage
        exit 1
      fi
      context="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 1
      ;;
  esac
done
if [[ -z "${context}" ]]; then
  usage
  exit 1
fi

for tool in kubectl jq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    printf '%s is required.\n' "${tool}" >&2
    exit 1
  fi
done

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

summary() {
  printf '%s\n' "$1"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >>"${GITHUB_STEP_SUMMARY}" || true
  fi
}

inconclusive() {
  summary "INCONCLUSIVE: $1"
  exit 3
}

# read_json <output-file> <kubectl get arguments…>
read_json() {
  local output="$1"
  shift
  kubectl --context "${context}" get "$@" --output json >"${output}" 2>/dev/null &&
    jq -e 'type == "object"' "${output}" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# 1. The policy must be installed, Ready and enforcing. A policy in Audit admits everything, so
#    every "must be refused" probe would fail for a reason that is not a defect in the rule.
# ---------------------------------------------------------------------------
read_json "${work_dir}/policy.json" clusterpolicy "${policy}" ||
  inconclusive 'the policy could not be read.'
jq -e '[.status.conditions[]? | select(.type == "Ready") | .status] == ["True"]' \
  "${work_dir}/policy.json" >/dev/null ||
  inconclusive 'the policy is not Ready.'
jq -e '
  (.spec.rules // []) as $rules
  | ($rules | length) > 0
    and all($rules[]; (.validate.failureAction // "") == "Enforce")' \
  "${work_dir}/policy.json" >/dev/null ||
  inconclusive 'the policy is not set to Enforce on every rule.'

# ---------------------------------------------------------------------------
# 2. The Team objects. Every one must carry a numeric identity: without it there is no
#    "unchanged" value to send and no identity to swap.
# ---------------------------------------------------------------------------
read_json "${work_dir}/teams.json" "${team_resource}" --namespace "${team_namespace}" ||
  inconclusive 'the Team objects could not be read.'
jq -r --arg key "${identity_annotation}" '
  .items[]? | [.metadata.name // "", .metadata.annotations[$key] // ""] | @tsv' \
  "${work_dir}/teams.json" >"${work_dir}/teams.tsv" ||
  inconclusive 'the Team objects could not be parsed.'
team_count="$(grep -c . "${work_dir}/teams.tsv" || true)"
[[ "${team_count}" -gt 0 ]] || inconclusive 'there is no Team object to probe.'
while IFS=$'\t' read -r name identity; do
  [[ "${name}" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] ||
    inconclusive 'a Team object has a name this probe will not send.'
  [[ "${identity}" =~ ^[0-9]+$ ]] ||
    inconclusive "Team ${name} carries no numeric identity yet."
  [[ "${identity}" != "${foreign_identity}" ]] ||
    inconclusive "Team ${name} carries the identity this probe uses as the foreign one."
done <"${work_dir}/teams.tsv"

# ---------------------------------------------------------------------------
# 3. The provider's ServiceAccount, read from its running pod because the name changes with every
#    provider revision.
# ---------------------------------------------------------------------------
read_json "${work_dir}/pods.json" pods --namespace "${provider_namespace}" ||
  inconclusive 'the provider pods could not be read.'
provider_accounts="$(jq -r --arg prefix "${provider_prefix}" '
  [.items[]?
    | select(.status.phase == "Running" and .metadata.deletionTimestamp == null)
    | .spec.serviceAccountName // ""
    | select(startswith($prefix))] | unique | .[]' "${work_dir}/pods.json")" ||
  inconclusive 'the provider pods could not be parsed.'
[[ -n "${provider_accounts}" && "$(grep -c . <<<"${provider_accounts}")" -eq 1 ]] ||
  inconclusive 'there is not exactly one running GitHub provider ServiceAccount.'
[[ "${provider_accounts}" =~ ^[a-z0-9-]+$ ]] ||
  inconclusive 'the provider ServiceAccount has a name this probe will not send.'
readonly provider="system:serviceaccount:${provider_namespace}:${provider_accounts}"

# ---------------------------------------------------------------------------
# One dry-run request, classified from the API server's answer.
#   admitted   the request passed admission and would have been stored
#   exists     the request passed admission and was then refused as AlreadyExists
#   refused    the request was refused and the answer names THIS policy
#   unknown    anything else — never counted either way
# ---------------------------------------------------------------------------
outcome=''
classify() {
  local rc="$1" errors="$2"
  if [[ "${rc}" -eq 0 ]]; then
    outcome='admitted'
  elif grep -Fq 'denied the request' "${errors}" && grep -Fq "${policy}" "${errors}"; then
    outcome='refused'
  elif grep -Fq 'AlreadyExists' "${errors}" || grep -Fq 'already exists' "${errors}"; then
    outcome='exists'
  else
    outcome='unknown'
  fi
}

# dry_patch <as> <team> <identity>
dry_patch() {
  local rc=0 patch
  patch="$(jq -cn --arg key "${identity_annotation}" --arg value "$3" \
    '{metadata: {annotations: {($key): $value}}}')"
  kubectl --context "${context}" patch "${team_resource}" "$2" --namespace "${team_namespace}" \
    --as "$1" --dry-run=server --type merge --patch "${patch}" \
    >/dev/null 2>"${work_dir}/errors" || rc=$?
  classify "${rc}" "${work_dir}/errors"
}

# dry_create <as> <team> <identity>: the live object's own spec under the given identity.
dry_create() {
  local rc=0
  jq --arg name "$2" --arg key "${identity_annotation}" --arg value "$3" '
    .items[] | select(.metadata.name == $name)
    | {apiVersion, kind,
       metadata: {name: .metadata.name, namespace: .metadata.namespace,
                  annotations: {($key): $value}},
       spec}' "${work_dir}/teams.json" >"${work_dir}/manifest.json"
  kubectl --context "${context}" create --as "$1" --dry-run=server \
    --filename "${work_dir}/manifest.json" >/dev/null 2>"${work_dir}/errors" || rc=$?
  classify "${rc}" "${work_dir}/errors"
}

broken=0
unattributed=0
# expect <team> <probe> <wanted outcome> — judges ${outcome}.
expect() {
  if [[ "${outcome}" == "$3" ]]; then
    printf '  ok            %-12s %-10s %s\n' "$1" "$2" "${outcome}"
  elif [[ "${outcome}" == 'unknown' ]]; then
    unattributed=$((unattributed + 1))
    printf '  UNATTRIBUTED  %-12s %-10s wanted %s\n' "$1" "$2" "$3"
  elif [[ "$3" == 'exists' && "${outcome}" == 'admitted' ]]; then
    # A re-create that is admitted outright means the object vanished mid-probe.
    unattributed=$((unattributed + 1))
    printf '  UNATTRIBUTED  %-12s %-10s wanted %s, got %s\n' "$1" "$2" "$3" "${outcome}"
  else
    broken=$((broken + 1))
    printf '  WRONG         %-12s %-10s wanted %s, got %s\n' "$1" "$2" "$3" "${outcome}"
  fi
}

printf 'Probing %s Team object(s) with server-side dry-run requests.\n' "${team_count}"
while IFS=$'\t' read -r name identity; do
  dry_patch "${applier}" "${name}" "${identity}"
  expect "${name}" unchanged admitted

  dry_patch "${applier}" "${name}" "${foreign_identity}"
  expect "${name}" foreign refused

  while IFS=$'\t' read -r other_name other_identity; do
    [[ "${other_name}" != "${name}" && "${other_identity}" != "${identity}" ]] || continue
    dry_patch "${applier}" "${name}" "${other_identity}"
    expect "${name}" swapped refused
  done <"${work_dir}/teams.tsv"

  dry_create "${applier}" "${name}" "${foreign_identity}"
  expect "${name}" re-create refused

  dry_create "${applier}" "${name}" "${identity}"
  expect "${name}" adoption exists

  dry_patch "${provider}" "${name}" "${foreign_identity}"
  expect "${name}" provider admitted
done <"${work_dir}/teams.tsv"

if [[ "${broken}" -gt 0 ]]; then
  summary "NOT-ENFORCED: ${broken} probe(s) got the opposite answer from production admission."
  exit 2
fi
if [[ "${unattributed}" -gt 0 ]]; then
  inconclusive "${unattributed} probe(s) got an answer that does not name the policy."
fi
summary "ENFORCED: production admission refused every foreign identity and admitted every legitimate write, for ${team_count} Team object(s). Nothing was stored."
