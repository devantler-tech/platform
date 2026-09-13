#!/usr/bin/env bash
# Pin the trigger set of .github/workflows/validate-image-verifier-liveness.yaml
# (devantler-tech/platform#3336).
#
# WHY THIS EXISTS. The workflow's two jobs are the daily regression signal for
# node-level image signature enforcement, and they only matter if they run.
# Every way of switching them off is silent:
#   * a removed `schedule:` leaves `main` green with nothing checked;
#   * a job-level `if: github.event_name == 'workflow_dispatch'` skips the
#     scheduled run while the run itself still reports success;
#   * a missing `environment: prod` resolves KUBE_CONFIG and TALOS_CONFIG to the
#     empty string, so the check cannot reach a single node;
#   * a `pull_request` or `push` trigger runs a prod-environment job from a ref
#     the environment refuses, which turns the signal into permanent noise.
# None of those fails CI by itself.
#
# Every assertion is ABLATED: a copy of the workflow is mutated in exactly one
# place and the check must fail naming THAT assertion. A check that cannot fail
# is not a check, and one that fails for a different reason proves nothing.
#
# yq (mikefarah v4) reads the YAML; no network, no secrets. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/validate-image-verifier-liveness.yaml"
readonly fleet_job='validate-image-verifier-liveness'
readonly inventory_job='inventory-first-party-image-signatures'

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() { rm -rf "${work_dir}"; }
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq is required'
[ -f "${workflow}" ] || fail "workflow not found: ${workflow}"

# q <file> <expr> → the expression's value, or the empty string when it is absent.
q() {
  yq -r "$2" "$1" | sed 's/^null$//'
}

# ── The check ─────────────────────────────────────────────────────────────────
# check <file>: prints "ok" and exits 0, or prints "VIOLATION <id>: <why>" and
# exits 1. The ablations key on the id, so a mutation that trips a DIFFERENT
# assertion than the one it targets is caught as a wrongly-attributed proof.
check() {
  local f="$1" cron minute hour dom month dow extra keys job offenders

  # T1 — runs exactly once a day, every day.
  [ "$(yq -r '.on.schedule | length' "$f")" = '1' ] ||
    { printf 'VIOLATION T1: expected exactly one schedule entry\n'; return 1; }
  cron="$(q "$f" '.on.schedule[0].cron')"
  minute='' hour='' dom='' month='' dow='' extra=''
  read -r minute hour dom month dow extra <<<"${cron}"
  case "${minute}" in
    '' | *[!0-9]*) printf 'VIOLATION T1: cron minute is not a fixed number (got "%s")\n' "${cron}"; return 1 ;;
  esac
  case "${hour}" in
    '' | *[!0-9]*) printf 'VIOLATION T1: cron hour is not a fixed number (got "%s")\n' "${cron}"; return 1 ;;
  esac
  if [ "${minute}" -gt 59 ] || [ "${hour}" -gt 23 ]; then
    printf 'VIOLATION T1: cron minute or hour out of range (got "%s")\n' "${cron}"
    return 1
  fi
  if [ "${dom}" != '*' ] || [ "${month}" != '*' ] || [ "${dow}" != '*' ] || [ -n "${extra}" ]; then
    printf 'VIOLATION T1: cron is not a five-field daily schedule (got "%s")\n' "${cron}"
    return 1
  fi

  # T2 — still runnable on demand.
  [ "$(yq -r '(.on // {}) | has("workflow_dispatch")' "$f")" = 'true' ] ||
    { printf 'VIOLATION T2: no workflow_dispatch trigger\n'; return 1; }

  # T3 — no other trigger. Both jobs read prod through an environment that only
  # admits protected branches.
  keys="$(yq -r '(.on // {}) | keys | sort | join(",")' "$f")"
  [ "${keys}" = 'schedule,workflow_dispatch' ] ||
    { printf 'VIOLATION T3: triggers are "%s", expected schedule,workflow_dispatch\n' "${keys}"; return 1; }

  # T0 — both jobs exist, so T4 and T5 below cannot pass vacuously.
  for job in "${fleet_job}" "${inventory_job}"; do
    [ "$(yq -r ".jobs | has(\"${job}\")" "$f")" = 'true' ] ||
      { printf 'VIOLATION T0: job %s is missing\n' "${job}"; return 1; }
  done

  # T4 — no job carries a condition. Any job-level `if:` can skip the scheduled
  # run while the run reports success.
  offenders="$(yq -r '.jobs | to_entries | map(select(.value | has("if"))) | map(.key) | join(",")' "$f")"
  [ -z "${offenders}" ] ||
    { printf 'VIOLATION T4: job-level if: on %s\n' "${offenders}"; return 1; }

  # T5 — every job runs in environment prod, where KUBE_CONFIG and TALOS_CONFIG live.
  offenders="$(yq -r '.jobs | to_entries | map(select(.value.environment != "prod")) | map(.key) | join(",")' "$f")"
  [ -z "${offenders}" ] ||
    { printf 'VIOLATION T5: not in environment prod: %s\n' "${offenders}"; return 1; }

  printf 'ok\n'
}

# ── Control: the committed workflow passes ────────────────────────────────────
out="$(check "${workflow}")" || fail "the committed workflow violates its own contract: ${out}"
[ "${out}" = 'ok' ] || fail "unexpected check output on the committed workflow: ${out}"

# ── Ablations: each mutation must trip EXACTLY the assertion it targets ───────
ablations=0
# ablate <id> <description> <yq mutation>
ablate() {
  local id="$1" description="$2" mutation="$3" copy out
  copy="${work_dir}/ablation-${ablations}.yaml"
  cp "${workflow}" "${copy}"
  yq -i "${mutation}" "${copy}"
  # The mutation must have changed the file, or the ablation proved nothing.
  if cmp -s "${workflow}" "${copy}"; then
    fail "ablation ${id} (${description}) did not change the workflow — vacuous"
  fi
  if out="$(check "${copy}")"; then
    fail "ablation ${id} (${description}) was NOT caught (check printed: ${out})"
  fi
  case "${out}" in
    "VIOLATION ${id}:"*) ;;
    *) fail "ablation ${id} (${description}) tripped the wrong assertion: ${out}" ;;
  esac
  ablations=$((ablations + 1))
}

ablate T1 'schedule removed' 'del(.on.schedule)'
ablate T1 'schedule made weekly' '.on.schedule[0].cron = "43 4 * * 1"'
ablate T1 'schedule made hourly' '.on.schedule[0].cron = "43 * * * *"'
ablate T1 'schedule on some days of the month' '.on.schedule[0].cron = "43 4 1-15 * *"'
ablate T1 'second schedule entry' '.on.schedule += [{"cron": "43 16 * * *"}]'
ablate T1 'six-field cron' '.on.schedule[0].cron = "43 4 * * * *"'
ablate T1 'hour out of range' '.on.schedule[0].cron = "43 24 * * *"'
ablate T2 'workflow_dispatch removed' 'del(.on.workflow_dispatch)'
ablate T3 'pull_request trigger added' '.on.pull_request = {}'
ablate T3 'push trigger added' '.on.push = {"branches": ["main"]}'
ablate T0 'fleet job removed' "del(.jobs[\"${fleet_job}\"])"
ablate T0 'inventory job removed' "del(.jobs[\"${inventory_job}\"])"
ablate T4 'dispatch-only guard on the fleet job' \
  ".jobs[\"${fleet_job}\"].if = \"github.event_name == 'workflow_dispatch'\""
ablate T4 'schedule-skipping guard on the inventory job' \
  ".jobs[\"${inventory_job}\"].if = \"github.event_name != 'schedule'\""
ablate T5 'fleet job lost environment prod' "del(.jobs[\"${fleet_job}\"].environment)"
ablate T5 'inventory job in another environment' ".jobs[\"${inventory_job}\"].environment = \"staging\""

printf 'test-validate-image-verifier-liveness-workflow: 1 control + %d ablations passed\n' "${ablations}"
