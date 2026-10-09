#!/usr/bin/env bash
# Helpers for the protected canary. Loaded only after deployment identity checks.
# API diagnostics and job metadata stay in the caller's private scratch directory.
# The canary supplies these checked variables and kc/quiet/deadline helpers.
# shellcheck disable=SC2154

readonly arc_origin_key=platform.devantler.tech/arc-runtime-origin
readonly arc_probe_uid_key=platform.devantler.tech/arc-runtime-probe-uid

arc_actions_read() {
  [[ -n "${GH_TOKEN:-}" ]] || return 1
  timeout 60s gh api "$1" --hostname github.com --method GET \
    -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
    "${@:3}" >"$2" 2>"$scratch/actions-error" || return 1
  jq -e -s 'length == 1' "$2" >/dev/null
}

arc_attempt_identity() {
  local origin=$1 terminal=$2 run attempt workflow head job job_name
  run=$(jq -er '.run' "$origin")
  attempt=$(jq -er '.attempt' "$origin")
  workflow=$(jq -er '.workflow' "$origin")
  head=$(jq -er '.head' "$origin")
  job=$(jq -er '.job' "$origin")
  job_name=$(jq -er '.job_name' "$origin")
  arc_actions_read "repos/devantler-tech/platform/actions/runs/$run/attempts/$attempt" "$scratch/attempt.json"
  jq -e --arg run "$run" --arg attempt "$attempt" --arg workflow "$workflow" --arg head "$head" --arg terminal "$terminal" '
    (.id | tostring) == $run and (.run_attempt | tostring) == $attempt and .head_sha == $head and
    .path == $workflow and .repository.full_name == "devantler-tech/platform" and
    .head_repository.full_name == "devantler-tech/platform" and
    ((.path == ".github/workflows/ci.yaml" and .event == "merge_group") or
     (.path == ".github/workflows/cd.yaml" and .event == "workflow_dispatch")) and
    (if $terminal == "true" then .status == "completed" and .conclusion != null
     else .status == "in_progress" end)
  ' "$scratch/attempt.json" >/dev/null
  arc_actions_read "repos/devantler-tech/platform/actions/runs/$run/attempts/$attempt/jobs?per_page=100" \
    "$scratch/attempt-jobs.json" --paginate --slurp
  # Pagination must be complete and consistent, with no duplicate job identity.
  jq -e --arg run "$run" --arg head "$head" --arg job "$job" --arg name "$job_name" --arg terminal "$terminal" '
    length > 0 and length <= 25 and
    (.[0].total_count as $total | all(.[]; .total_count == $total) and
     ([.[].jobs[]] | length) == $total) and
    ([.[].jobs[].id] | length == (unique | length)) and
    ([.[].jobs[] | select((.id | tostring) == $job and .name == $name and
      (.run_id | tostring) == $run and .head_sha == $head and
      (if $terminal == "true" then .status == "completed" and .conclusion != null and
        (.completed_at | type == "string" and length > 0)
       else .status == "in_progress" end))] | length) == 1
  ' "$scratch/attempt-jobs.json" >/dev/null
}

arc_current_origin() {
  local workflow job_name source job
  [[ "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ &&
     "$GITHUB_RUN_ID" =~ ^[1-9][0-9]{0,14}$ ]] || return 1
  case "${GITHUB_WORKFLOW_REF:-}:${GITHUB_JOB:-}" in
    devantler-tech/platform/.github/workflows/ci.yaml@*:deploy-prod)
      workflow=.github/workflows/ci.yaml; job_name='🚀 Deploy to Prod' ;;
    devantler-tech/platform/.github/workflows/cd.yaml@*:deploy-prod)
      workflow=.github/workflows/cd.yaml; job_name='🚀 Deploy to Production' ;;
    devantler-tech/platform/.github/workflows/ci.yaml@*:heal-prod-on-failure)
      workflow=.github/workflows/ci.yaml
      job_name='🩹 Heal Prod (restore main after an unsuccessful or evicted merge-group deploy)' ;;
    *) return 1 ;;
  esac
  source=$(git rev-parse HEAD)
  [[ "$source" =~ ^[0-9a-f]{40}$ ]] || return 1
  [[ "$source" == "$GITHUB_SHA" || "$GITHUB_JOB" == heal-prod-on-failure ]] || return 1
  arc_actions_read "repos/devantler-tech/platform/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100" \
    "$scratch/current-jobs.json" --paginate --slurp
  job=$(jq -er --arg name "$job_name" --arg run "$GITHUB_RUN_ID" --arg head "$GITHUB_SHA" '
    [.[].jobs[] | select(.name == $name and (.run_id | tostring) == $run and
      .head_sha == $head and .status == "in_progress")] |
    select(length == 1) | .[0].id | tostring | select(test("^[1-9][0-9]{0,14}$"))
  ' "$scratch/current-jobs.json")
  jq -n --arg workflow "$workflow" --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" \
    --arg head "$GITHUB_SHA" --arg source "$source" --arg manifest "$PLATFORM_MANIFEST_DIGEST" \
    --arg job "$job" --arg job_name "$job_name" '{repository:"devantler-tech/platform",
      workflow:$workflow,run:$run,attempt:$attempt,head:$head,source:$source,
      manifest:$manifest,job:$job,job_name:$job_name}' >"$scratch/current-origin.json"
  arc_attempt_identity "$scratch/current-origin.json" false
}

arc_retained_fence_matches() {
  kc -n "$namespace" get resourcequota "$fence_name" -o json >"$scratch/retained-fence-current.json" || return 1
  [[ -s "$scratch/retained-fence-current.json" ]] || return 1
  jq -e --slurpfile before "$scratch/retained-fence.json" '
    .metadata.uid == $before[0].metadata.uid and
    .metadata.labels == $before[0].metadata.labels and
    .metadata.annotations == $before[0].metadata.annotations and
    .metadata.deletionTimestamp == null and (.metadata.ownerReferences // [] | length) == 0 and
    .spec == {hard:{pods:"0"},scopes:["NotTerminating"]} and .status.hard == {pods:"0"}
  ' "$scratch/retained-fence-current.json" >/dev/null
}

arc_recover_admission_fence() {
  local owner run attempt recorded_uid current deadline remaining
  kc -n "$namespace" get resourcequota "$fence_name" --ignore-not-found -o json >"$scratch/retained-fence.json"
  [[ -s "$scratch/retained-fence.json" ]] || return 0
  jq -e --arg name "$fence_name" --arg ns "$namespace" '
    .metadata.name == $name and .metadata.namespace == $ns and
    (.metadata.uid | type == "string" and length > 0) and
    (.metadata.resourceVersion | type == "string" and length > 0) and
    .metadata.deletionTimestamp == null and (.metadata.ownerReferences // [] | length) == 0 and
    .spec == {hard:{pods:"0"},scopes:["NotTerminating"]} and .status.hard == {pods:"0"}
  ' "$scratch/retained-fence.json" >/dev/null
  jq -er --arg key "$arc_origin_key" '.metadata.annotations[$key] | fromjson' \
    "$scratch/retained-fence.json" >"$scratch/prior-origin.json"
  jq -e 'type == "object" and .repository == "devantler-tech/platform" and
    (.run | test("^[1-9][0-9]{0,14}$")) and (.attempt | test("^[1-9][0-9]{0,2}$")) and
    (.job | test("^[1-9][0-9]{0,14}$")) and
    (.head | test("^[0-9a-f]{40}$")) and (.source | test("^[0-9a-f]{40}$")) and
    (.manifest | test("^sha256:[0-9a-f]{64}$")) and
    ((.workflow == ".github/workflows/ci.yaml" and .job_name == "🚀 Deploy to Prod" and .source == .head) or
     (.workflow == ".github/workflows/cd.yaml" and .job_name == "🚀 Deploy to Production" and .source == .head) or
     (.workflow == ".github/workflows/ci.yaml" and
      .job_name == "🩹 Heal Prod (restore main after an unsuccessful or evicted merge-group deploy)"))
  ' "$scratch/prior-origin.json" >/dev/null
  run=$(jq -r '.run' "$scratch/prior-origin.json")
  attempt=$(jq -r '.attempt' "$scratch/prior-origin.json")
  owner="arc-proof-$run-$attempt"
  [[ "$owner" != "$probe" ]] || return 1
  jq -e --arg key "$ownership_label" --arg owner "$owner" '.metadata.labels[$key] == $owner' \
    "$scratch/retained-fence.json" >/dev/null
  # Whole-attempt death is deliberately required. Same-run Heal Prod remains
  # HOLD; its recovery support must be staged on inactive main before activation.
  arc_attempt_identity "$scratch/prior-origin.json" true
  verify_runner_deadlines
  arc_retained_fence_matches
  current=$(kc -n "$namespace" get pod "$owner" --ignore-not-found -o json)
  if [[ -n "$current" ]]; then
    recorded_uid=$(jq -er --arg key "$arc_probe_uid_key" \
      '.metadata.annotations[$key] | select(type == "string" and length > 0)' "$scratch/retained-fence.json")
    jq -e --arg name "$owner" --arg ns "$namespace" --arg key "$ownership_label" --arg uid "$recorded_uid" '
      .metadata.name == $name and .metadata.namespace == $ns and .metadata.labels[$key] == $name and
      .metadata.uid == $uid and (.metadata.ownerReferences // [] | length) == 0 and
      .spec.activeDeadlineSeconds == 1200 and .spec.restartPolicy == "Never"
    ' <<<"$current" >/dev/null
    jq -n --arg uid "$recorded_uid" '{apiVersion:"v1",kind:"DeleteOptions",preconditions:{uid:$uid},
      gracePeriodSeconds:5,propagationPolicy:"Background"}' >"$scratch/prior-delete.json"
    arc_retained_fence_matches
    quiet kc delete --raw="/api/v1/namespaces/$namespace/pods/$owner" -f "$scratch/prior-delete.json"
    quiet kc -n "$namespace" wait "pod/$owner" --for=delete --timeout=25s
  fi
  current=$(kc -n "$namespace" get pod "$owner" --ignore-not-found -o json)
  [[ -z "$current" ]]
  kc -n "$namespace" get pods -l "$ownership_label=$owner" -o json >"$scratch/prior-probes.json"
  jq -e '(.metadata.continue // "") == "" and (.items | length == 0)' "$scratch/prior-probes.json" >/dev/null
  deadline=$((SECONDS + 1200))
  while ((SECONDS < deadline)); do
    arc_retained_fence_matches
    remaining=$(kc get nodes -o json)
    if jq -e '(.metadata.continue // "") == "" and (.items | all(.[];
      .metadata.labels["platform.devantler.tech/ci-runner"] != "enabled" and
      (.metadata.name | startswith("autoscale-arc-runners-") | not)))' <<<"$remaining" >/dev/null; then break; fi
    sleep 10
  done
  jq -e '(.metadata.continue // "") == "" and (.items | all(.[];
    .metadata.labels["platform.devantler.tech/ci-runner"] != "enabled" and
    (.metadata.name | startswith("autoscale-arc-runners-") | not)))' <<<"$remaining" >/dev/null
  arc_attempt_identity "$scratch/prior-origin.json" true
  verify_runner_deadlines
  arc_retained_fence_matches
  current=$(kc -n "$namespace" get pod "$owner" --ignore-not-found -o json)
  [[ -z "$current" ]]
  kc -n "$namespace" get pods -l "$ownership_label=$owner" -o json >"$scratch/prior-probes.json"
  jq -e '(.metadata.continue // "") == "" and (.items | length == 0)' "$scratch/prior-probes.json" >/dev/null
  jq '{apiVersion:"v1",kind:"DeleteOptions",preconditions:{uid:.metadata.uid,
    resourceVersion:.metadata.resourceVersion},propagationPolicy:"Background"}' \
    "$scratch/retained-fence-current.json" >"$scratch/prior-delete-fence.json"
  quiet kc delete --raw="/api/v1/namespaces/$namespace/resourcequotas/$fence_name" -f "$scratch/prior-delete-fence.json"
  current=$(kc -n "$namespace" get resourcequota "$fence_name" --ignore-not-found -o json)
  [[ -z "$current" ]]
  printf 'ARC acceptance: abandoned admission fence recovered\n'
}

arc_record_probe_uid() {
  verify_admission_fence
  jq -n --arg uid "$fence_uid" --arg version "$(jq -r '.metadata.resourceVersion' "$scratch/live-fence.json")" \
    --arg probe_uid "$probe_uid" '[{op:"test",path:"/metadata/uid",value:$uid},
      {op:"test",path:"/metadata/resourceVersion",value:$version},
      {op:"add",path:"/metadata/annotations/platform.devantler.tech~1arc-runtime-probe-uid",value:$probe_uid}]' \
    >"$scratch/probe-uid-patch.json"
  quiet kc -n "$namespace" patch resourcequota "$fence_name" --type=json --patch-file "$scratch/probe-uid-patch.json"
  verify_admission_fence
  jq -e --arg key "$arc_probe_uid_key" --arg uid "$probe_uid" '.metadata.annotations[$key] == $uid' \
    "$scratch/live-fence.json" >/dev/null
}
