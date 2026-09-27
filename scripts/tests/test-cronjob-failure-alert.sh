#!/usr/bin/env bash
# Contract for the reconcile-CronJob failure detector (#2915).
#
# Replays consecutive failures, a single absorbed failure, a stalled loop, a
# missing CronJob, a suspended CronJob and every API failure against a stubbed
# Kubernetes API and delivery, under both bash and POSIX sh, so the fire, each
# quiet verdict and each silent-zero guard are pinned by behaviour.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly bundle_dir="${root_dir}/k8s/bases/components/coroot-cronjob-failure-alert"
readonly manifest="${bundle_dir}/cron-job-cronjob-failure-alert.yaml"
readonly role="${bundle_dir}/role-cronjob-failure-alert-umami.yaml"
readonly binding="${bundle_dir}/role-binding-cronjob-failure-alert-umami.yaml"
readonly kubescape_role="${bundle_dir}/role-cronjob-failure-alert-kubescape.yaml"
readonly kubescape_binding="${bundle_dir}/role-binding-cronjob-failure-alert-kubescape.yaml"
readonly kustomization="${bundle_dir}/kustomization.yaml"
readonly hetzner_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
readonly alerting_doc="${root_dir}/docs/dr/alerting.md"
readonly documented_manifest='bases/components/coroot-cronjob-failure-alert/cron-job-cronjob-failure-alert.yaml'
readonly real_webhook='https://hooks.test.invalid/delivery-target'
readonly placeholder_webhook='https://example.invalid/no-slack-configured'

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$1"; }

for tool in yq jq; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

container_path='.spec.jobTemplate.spec.template.spec.containers[0]'
pod_path='.spec.jobTemplate.spec.template.spec'
script_body="$(yq eval "${container_path}.command[2]" "$manifest")"
if [ -z "$script_body" ] || [ "$script_body" = null ]; then
  fail 'could not extract detector script'
fi
env_value() { yq eval "${container_path}.env[] | select(.name == \"$1\") | .value" "$manifest"; }

# ---- Static contract -------------------------------------------------------
[ "$(yq eval '.metadata.namespace' "$role")" = umami ] || fail 'Role must be namespaced to umami'
[ "$(yq eval '[.rules[] | select(.resources[] == "cronjobs") | .verbs[]] | join(",")' "$role")" = get ] ||
  fail 'the CronJob grant must be get only'
[ "$(yq eval '[.rules[] | select(.resources[] == "cronjobs") | .resourceNames[]] | join(",")' "$role")" = umami-provision-tenants ] ||
  fail 'the CronJob grant must name only the watched CronJob'
[ "$(yq eval '[.rules[] | select(.resources[] == "jobs") | .verbs[]] | join(",")' "$role")" = list ] ||
  fail 'the Job grant must be list only'
[ "$(yq eval '[.rules[].resources[]] | sort | join(",")' "$role")" = cronjobs,jobs ] ||
  fail 'Role may read only cronjobs and jobs'
[ "$(yq eval '[.rules[].apiGroups[]] | unique | join(",")' "$role")" = batch ] || fail 'Role may use only the batch API group'
[ "$(yq eval '.roleRef.kind' "$binding")" = Role ] || fail 'binding must reference the namespaced Role'
[ "$(yq eval '.subjects | length' "$binding")" = 1 ] || fail 'binding must have exactly one subject'
[ "$(yq eval '.subjects[0].namespace + "/" + .subjects[0].name' "$binding")" = observability/cronjob-failure-alert ] ||
  fail 'binding must grant only the detector ServiceAccount'
pass 'RBAC is get on the one watched CronJob and list on Jobs in umami'

[ "$(env_value WATCH)" = 'umami/umami-provision-tenants:2:3600 kubescape/kubescape-hostdata-cleanup:1:93600' ] ||
  fail 'watch list must include both the Umami and Kubescape reconcile loops'
[ "$(yq eval '.metadata.namespace' "$kubescape_role")" = kubescape ] || fail 'Kubescape Role must be namespaced to kubescape'
[ "$(yq eval '[.rules[] | select(.resources[] == "cronjobs") | .resourceNames[]] | join(",")' "$kubescape_role")" = kubescape-hostdata-cleanup ] ||
  fail 'the Kubescape CronJob grant must name only the host-data cleanup'
[ "$(yq eval '[.rules[] | select(.resources[] == "cronjobs") | .verbs[]] | join(",")' "$kubescape_role")" = get ] ||
  fail 'the Kubescape CronJob grant must be get only'
[ "$(yq eval '[.rules[] | select(.resources[] == "jobs") | .verbs[]] | join(",")' "$kubescape_role")" = list ] ||
  fail 'the Kubescape Job grant must be list only'
[ "$(yq eval '.subjects[0].namespace + "/" + .subjects[0].name' "$kubescape_binding")" = observability/cronjob-failure-alert ] ||
  fail 'Kubescape binding must grant only the detector ServiceAccount'
pass 'Kubescape cleanup has the same narrow read-only alert grant'

# Every watched namespace must carry its own grant, or the check fails at 403.
for target in $(env_value WATCH); do
  target=${target%%:*}
  ns=${target%%/*}
  grep -Fq "role-cronjob-failure-alert-${ns}.yaml" "$kustomization" ||
    fail "watched namespace ${ns} has no Role in the component"
  grep -Fq "role-binding-cronjob-failure-alert-${ns}.yaml" "$kustomization" ||
    fail "watched namespace ${ns} has no RoleBinding in the component"
done
for resource in service-account secret cron-job; do
  grep -Fq "${resource}-cronjob-failure-alert.yaml" "$kustomization" ||
    fail "$resource manifest is missing from the component"
done
grep -Fq '../../../../bases/components/coroot-cronjob-failure-alert' "$hetzner_kustomization" ||
  fail 'the Hetzner composition must enable the alert'
pass 'the watched namespaces and the component composition agree'

grep -Fq "\`${documented_manifest}\`" "$alerting_doc" || fail 'the alerting runbook does not point at the manifest'
[ "$(yq eval '.metadata.annotations["kustomize.toolkit.fluxcd.io/substitute"]' "$manifest")" = disabled ] ||
  fail 'CronJob must disable Flux substitution for its shell variables'
[ "$(yq eval "${pod_path}.volumes[] | select(.name == \"webhook\") | .secret.defaultMode" "$manifest")" = 288 ] ||
  fail 'webhook Secret mode must be 0440'
if grep -q secretKeyRef <<<"$(yq eval "${container_path}.env" "$manifest")"; then
  fail 'webhook must not be exposed through an environment variable'
fi
if grep -Eq 'curl.*"\$\{?WEBHOOK_URL\}?"' <<<"$script_body"; then
  fail 'webhook URL must not appear in curl argv'
fi
grep -Fq -- '--config -' <<<"$script_body" || fail 'webhook delivery must read its URL from stdin config'
pass 'webhook is mounted 0440 and never exposed in argv or environment'

# Feature flag: the detector is active (a manual run was validated against prod in platform#4035).
[ "$(yq eval '.spec.suspend' "$manifest")" = false ] || fail 'the detector must be active (validated against prod in platform#4035)'
pass 'the detector is active'

# ---- Behaviour -------------------------------------------------------------
work_root="$(mktemp -d "${TMPDIR:-/tmp}/cronjob-failure-alert.XXXXXXXXXX")"
trap 'rm -rf "$work_root"' EXIT
readonly now_epoch=1789200000
iso() { jq -rn --argjson t "$1" '$t | todate'; }

# job name status(Complete|Failed|Running) scheduled-seconds-ago [owner]
job() {
  local name=$1 status=$2 ago=$3 owner=${4:-umami-provision-tenants}
  jq -cn --arg name "$name" --arg status "$status" --arg owner "$owner" \
    --arg at "$(iso $((now_epoch - ago)))" --arg finished "$(iso $((now_epoch - ago + 30)))" '
      {metadata:{name:$name, creationTimestamp:$at,
        annotations:{"batch.kubernetes.io/cronjob-scheduled-timestamp":$at},
        ownerReferences:[{kind:"CronJob", name:$owner}]},
       status:(if $status == "Running" then {active:1} else
         {conditions:[{type:$status, status:"True", lastTransitionTime:$finished,
           reason:(if $status == "Failed" then "BackoffLimitExceeded" else "" end),
           message:(if $status == "Failed" then "Job has reached the specified backoff limit" else "" end)}]} end)}'
}
job_list() { jq -sc '{kind:"JobList", items:.}'; }
cronjob() { # created-seconds-ago [suspend]
  jq -cn --arg c "$(iso $((now_epoch - $1)))" --argjson s "${2:-false}" \
    '{kind:"CronJob", metadata:{name:"umami-provision-tenants", creationTimestamp:$c}, spec:{suspend:$s, failedJobsHistoryLimit:3}}'
}

setup_scenario() {
  local name=$1 webhook=${2:-$real_webhook}
  local dir="${work_root}/${name}"
  mkdir -p "${dir}/bin" "${dir}/sa" "${dir}/webhook" "${dir}/tmp"
  printf fake-ca >"${dir}/sa/ca.crt"
  printf fake-token >"${dir}/sa/token"
  printf '%s' "$webhook" >"${dir}/webhook/url"
  printf 200 >"${dir}/cronjob.code"
  cronjob 8640000 >"${dir}/cronjob.body"
  printf 200 >"${dir}/jobs.code"
  printf '' | job_list >"${dir}/jobs.body"
  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
dir=${SCENARIO_DIR}
printf '%s\n' "$*" >>"${dir}/curl-argv.log"
out='' prev='' url=''
for arg in "$@"; do
  [ "$prev" = -o ] && out=$arg
  case "$arg" in https://kubernetes.default.svc/*) url=$arg ;; esac
  prev=$arg
done
if [ -n "$out" ]; then
  case "$url" in
    */apis/batch/v1/namespaces/umami/cronjobs/umami-provision-tenants) key=cronjob ;;
    */apis/batch/v1/namespaces/umami/jobs) key=jobs ;;
    */apis/batch/v1/namespaces/kubescape/cronjobs/kubescape-hostdata-cleanup) key=cronjob ;;
    */apis/batch/v1/namespaces/kubescape/jobs) key=jobs ;;
    *) printf '%s\n' "unexpected URL $url" >>"${dir}/unexpected.log"; exit 93 ;;
  esac
  cp "${dir}/${key}.body" "$out"
  cat "${dir}/${key}.code"
  exit 0
fi
cat >"${dir}/curl-stdin.log"
printf delivered >"${dir}/delivered"
exit "${STUB_DELIVERY_EXIT:-0}"
STUB
  chmod +x "${dir}/bin/curl"
  printf '%s' "$dir"
}

shells=(bash sh)
run_scenario() { # dir [shell]
  local dir=$1 shell=${2:-bash}
  local patched="${dir}/script.sh"
  rm -f "${dir}/delivered" "${dir}/tmp/payload.json"
  {
    printf 'export NOW_EPOCH=%s\n' "${NOW_OVERRIDE:-$now_epoch}"
    printf 'export WATCH=%q\n' "${WATCH_OVERRIDE:-umami/umami-provision-tenants}"
    printf 'export FAILURE_THRESHOLD=%q\n' "${THRESHOLD_OVERRIDE:-$(env_value FAILURE_THRESHOLD)}"
    printf 'export STALE_SECONDS=%q\n' "$(env_value STALE_SECONDS)"
    printf 'SA_OVERRIDE=%q\n' "${dir}/sa"
    # shellcheck disable=SC2016
    printf '%s\n' "$script_body" |
      sed -e 's#^\( *\)SA=/var/run/secrets/kubernetes.io/serviceaccount$#\1SA="$SA_OVERRIDE"#' \
        -e 's#/tmp/#'"${dir}"'/tmp/#g' \
        -e 's#/etc/cronjob-failure-alert/url#'"${dir}"'/webhook/url#g'
  } >"$patched"
  # shellcheck disable=SC2016 # a literal string the harness wrote
  grep -q 'SA="$SA_OVERRIDE"' "$patched" || fail 'the harness did not redirect the ServiceAccount mount'
  PATH="${dir}/bin:${PATH}" SCENARIO_DIR="$dir" STUB_DELIVERY_EXIT="${STUB_DELIVERY_EXIT:-0}" \
    "$shell" "$patched" >"${dir}/stdout" 2>"${dir}/stderr"
}

expect_alert() { # dir label needle...
  local dir=$1 label=$2 shell
  shift 2
  for shell in "${shells[@]}"; do
    run_scenario "$dir" "$shell" || fail "${label} [${shell}] exited non-zero: $(cat "${dir}/stderr")"
    [ -f "${dir}/delivered" ] || fail "${label} [${shell}] produced no alert: $(cat "${dir}/stdout")"
    local payload needle
    payload="$(jq -r .text "${dir}/tmp/payload.json")"
    for needle in "$@"; do
      grep -Fq -- "$needle" <<<"$payload" || fail "${label} [${shell}] alert does not name '${needle}'"
    done
  done
  pass "$label"
}
expect_quiet() { # dir label stdout-needle
  local dir=$1 label=$2 needle=$3 shell
  for shell in "${shells[@]}"; do
    run_scenario "$dir" "$shell" || fail "${label} [${shell}] exited non-zero: $(cat "${dir}/stderr")"
    [ ! -f "${dir}/delivered" ] || fail "${label} [${shell}] alerted"
    grep -Fq -- "$needle" "${dir}/stdout" || fail "${label} [${shell}] did not report '${needle}': $(cat "${dir}/stdout")"
  done
  pass "$label"
}
expect_error() { # dir label stderr-needle
  local dir=$1 label=$2 needle=$3 shell
  for shell in "${shells[@]}"; do
    if run_scenario "$dir" "$shell"; then fail "${label} [${shell}] reported success"; fi
    [ ! -f "${dir}/delivered" ] || fail "${label} [${shell}] alerted instead of failing"
    grep -Fq -- "$needle" "${dir}/stderr" || fail "${label} [${shell}] did not report '${needle}': $(cat "${dir}/stderr")"
  done
  pass "$label"
}

# The shape #2915 is about: the newest two runs both failed.
dir="$(setup_scenario consecutive)"
{ job run-4 Failed 60; job run-3 Failed 960; job run-2 Complete 1860; } | job_list >"${dir}/jobs.body"
expect_alert "$dir" 'two consecutive failed runs alert, naming the target and both runs' \
  'umami/umami-provision-tenants' 'the last 2 runs all failed' run-4 run-3 BackoffLimitExceeded
grep -Fq 'run-2' "${dir}/tmp/payload.json" && fail 'a run outside the failure window was named'

dir="$(setup_scenario kubescape-single-failure)"
job cleanup-failed Failed 60 kubescape-hostdata-cleanup | job_list >"${dir}/jobs.body"
WATCH_OVERRIDE='kubescape/kubescape-hostdata-cleanup:1:93600' \
  expect_alert "$dir" 'the daily Kubescape cleanup alerts on its first failed run' \
    'kubescape/kubescape-hostdata-cleanup' 'the last 1 runs all failed' cleanup-failed

# Ordering is by schedule time, not list order: the same runs listed oldest
# first must give the same verdict.
dir="$(setup_scenario list-order)"
{ job run-2 Complete 1860; job run-3 Failed 960; job run-4 Failed 60; } | job_list >"${dir}/jobs.body"
expect_alert "$dir" 'the verdict does not depend on list order' run-4 run-3

# Control arm: the loop absorbs one failure by design (the weekly credential
# rotation). A single failure followed by success stays quiet ...
dir="$(setup_scenario absorbed)"
{ job run-3 Complete 60; job run-2 Failed 960; job run-1 Failed 1860; } | job_list >"${dir}/jobs.body"
expect_quiet "$dir" 'a success after failures clears the alert' 'healthy'
# ... and so does a lone newest failure.
dir="$(setup_scenario single)"
{ job run-2 Failed 60; job run-1 Complete 960; } | job_list >"${dir}/jobs.body"
expect_quiet "$dir" 'a single failed run is absorbed, not alerted' 'healthy'
# A still-running Job is not a finished run and must not break the streak.
dir="$(setup_scenario running)"
{ job run-3 Running 30; job run-2 Failed 960; job run-1 Failed 1860; } | job_list >"${dir}/jobs.body"
expect_alert "$dir" 'an in-flight run neither counts nor hides the streak' run-2 run-1
# Another CronJob's failures in the same namespace are not this loop's. They are
# the NEWEST runs here, so an owner filter that stopped matching would alert.
dir="$(setup_scenario foreign)"
{ job other-2 Failed 60 umami-other; job other-1 Failed 960 umami-other; job run-1 Complete 1800; } | job_list >"${dir}/jobs.body"
expect_quiet "$dir" "another CronJob's failures are ignored" 'healthy'

# Threshold boundary: with threshold 3, two failures are quiet and three alert.
dir="$(setup_scenario threshold)"
{ job run-3 Failed 60; job run-2 Failed 960; job run-1 Complete 1860; } | job_list >"${dir}/jobs.body"
THRESHOLD_OVERRIDE=3 expect_quiet "$dir" 'fewer failures than the threshold stay quiet' 'healthy'
{ job run-3 Failed 60; job run-2 Failed 960; job run-1 Failed 1860; } | job_list >"${dir}/jobs.body"
THRESHOLD_OVERRIDE=3 expect_alert "$dir" 'exactly the threshold of failures alerts' 'the last 3 runs all failed'

# Silence is not health: no run finishing within STALE_SECONDS alerts.
dir="$(setup_scenario stalled)"
{ job run-1 Complete 7200; } | job_list >"${dir}/jobs.body"
expect_alert "$dir" 'a loop with no finished run for over an hour alerts' 'no run has finished for more than 3600s' run-1
dir="$(setup_scenario never-ran)"
expect_alert "$dir" 'an old CronJob with no finished run at all alerts' 'no retained run has finished'
dir="$(setup_scenario fresh)"
cronjob 600 >"${dir}/cronjob.body"
expect_quiet "$dir" 'a brand-new CronJob gets one stale window before alerting' 'younger than 3600s'
dir="$(setup_scenario missing)"
printf 404 >"${dir}/cronjob.code"
printf '{"kind":"Status"}' >"${dir}/cronjob.body"
expect_alert "$dir" 'a watched CronJob that no longer exists alerts' 'the watched CronJob does not exist'
dir="$(setup_scenario suspended)"
cronjob 8640000 true >"${dir}/cronjob.body"
# A suspension stops the reconcile loop, so it is judged by the same staleness rule as any stall.
expect_alert "$dir" 'a suspended CronJob with no recent run alerts like any stalled loop' 'no retained run has finished'

# Silent-zero guards: a broken read fails the Job instead of reporting health.
dir="$(setup_scenario cronjob-403)"
printf 403 >"${dir}/cronjob.code"
expect_error "$dir" 'a forbidden CronJob read fails loudly' 'HTTP 403 reading CronJob'
dir="$(setup_scenario jobs-403)"
printf 403 >"${dir}/jobs.code"
expect_error "$dir" 'a forbidden Job list fails loudly' 'HTTP 403 listing Jobs'
dir="$(setup_scenario jobs-shape)"
printf '{"kind":"JobList","items":{}}' >"${dir}/jobs.body"
expect_error "$dir" 'a malformed Job list fails loudly' 'unexpected Job-list response shape'
dir="$(setup_scenario empty-watch)"
WATCH_OVERRIDE=' ' expect_error "$dir" 'an empty watch list fails instead of checking nothing' 'WATCH names no CronJob'
dir="$(setup_scenario bad-watch)"
WATCH_OVERRIDE='umami' expect_error "$dir" 'a malformed watch entry fails' "is not namespace/name"
dir="$(setup_scenario zero-threshold)"
THRESHOLD_OVERRIDE=0 expect_error "$dir" 'a zero threshold is refused' 'FAILURE_THRESHOLD must be a positive integer'
dir="$(setup_scenario padded-zero-threshold)"
THRESHOLD_OVERRIDE=00 expect_error "$dir" 'a zero-padded zero threshold is refused' 'FAILURE_THRESHOLD must be a positive integer'
dir="$(setup_scenario padded-zero-tuning)"
WATCH_OVERRIDE='umami/umami-provision-tenants:2:00' \
  expect_error "$dir" 'a zero-padded zero stale window is refused' 'has an invalid tuning value'
dir="$(setup_scenario oversized-tuning)"
WATCH_OVERRIDE='umami/umami-provision-tenants:2:9999999999999999999' \
  expect_error "$dir" 'an out-of-range stale window is refused' 'has an invalid tuning value'
dir="$(setup_scenario unobservable-threshold)"
WATCH_OVERRIDE='umami/umami-provision-tenants:4:3600' \
  expect_error "$dir" 'a threshold above the retained failed runs is refused' 'exceeds its failedJobsHistoryLimit 3'

# Delivery: the placeholder host never delivers; a failed delivery fails the Job.
dir="$(setup_scenario placeholder "$placeholder_webhook")"
{ job run-2 Failed 60; job run-1 Failed 960; } | job_list >"${dir}/jobs.body"
for shell in "${shells[@]}"; do
  run_scenario "$dir" "$shell" || fail "placeholder [${shell}] exited non-zero"
  [ ! -f "${dir}/delivered" ] || fail "placeholder [${shell}] delivered"
  grep -Fq 'No Slack webhook configured' "${dir}/stdout" || fail "placeholder [${shell}] did not say so"
done
pass 'the placeholder webhook is never delivered to'
dir="$(setup_scenario delivery-fails)"
{ job run-2 Failed 60; job run-1 Failed 960; } | job_list >"${dir}/jobs.body"
for shell in "${shells[@]}"; do
  if STUB_DELIVERY_EXIT=22 run_scenario "$dir" "$shell"; then fail "failed delivery [${shell}] reported success"; fi
done
grep -Fq "url = ${real_webhook}" "${dir}/curl-stdin.log" || fail 'the webhook URL was not passed on stdin'
if grep -Fq "$real_webhook" "${dir}/curl-argv.log"; then fail 'the webhook URL reached curl argv'; fi
pass 'a failed delivery fails the Job, and the URL never reaches argv'

printf 'PASS: cronjob-failure-alert fires on consecutive failures and stalls, absorbs a single failure, and never reports a silent zero\n'
