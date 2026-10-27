#!/usr/bin/env bash
# Reviewed workflow only: five finite, fixed Cilium CLI metric reads per ready agent (#4329).
# No API listener, setting, workload or policy is changed. Raw responses stay in a private
# temporary directory. Output is delayed until every read and identity comparison succeeds.
# Cilium v1.20.3 sources: cilium-dbg/cmd/metrics_list.go, pkg/metrics/cmd.go,
# pkg/metrics/metrics.go and pkg/rate/metrics/metrics.go. These are rolling gauges,
# not a latency distribution or proof of Coroot request attribution or recovery.
set -euo pipefail
if [[ "$#" -ne 2 || "$1" != --context || "$2" != admin@prod ]]; then
  printf 'Usage: %s --context admin@prod\n' "$(basename "$0")" >&2
  exit 1
fi
for tool in kubectl jq timeout head; do
  command -v "${tool}" >/dev/null 2>&1 || { printf 'Required diagnostic tool is unavailable.\n' >&2; exit 1; }
done
umask 077
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
inconclusive() { printf 'INCONCLUSIVE: %s\n' "$1"; exit 3; }

# Bound JSON responses and requests. Neither failed kubectl output nor parser diagnostics are public.
read_json() {
  local destination="$1"
  local -a read_status
  shift
  read_failure=COMMAND_FAILED
  if timeout 20s kubectl --context admin@prod --request-timeout 15s "$@" \
    2>"${work_dir}/read.stderr" | head -c 1048577 >"${destination}"; then
    read_status=("${PIPESTATUS[@]}")
  else
    read_status=("${PIPESTATUS[@]}")
  fi
  if [[ "$(wc -c <"${destination}")" -gt 1048576 ]]; then
    read_failure=OUTPUT_LIMIT
    return 1
  fi
  if [[ "${read_status[0]}" -ne 0 || "${read_status[1]}" -ne 0 ]]; then
    # Classify a bounded private prefix; publish only fixed constants, never matched text.
    head -c 8192 "${work_dir}/read.stderr" >"${work_dir}/read.stderr.prefix"
    if [[ "${read_status[0]}" -eq 124 ]]; then
      read_failure=DEADLINE_EXCEEDED
    elif LC_ALL=C grep -Eqi 'forbidden|unauthorized|you must be logged in' "${work_dir}/read.stderr.prefix"; then
      read_failure=ACCESS_DENIED
    elif LC_ALL=C grep -Eqi '(unknown|unrecognized) (flag|command)|flag provided but not defined' "${work_dir}/read.stderr.prefix"; then
      read_failure=CLI_REJECTED
    elif LC_ALL=C grep -Eq 'dial unix /var/run/cilium/shell\.sock:' "${work_dir}/read.stderr.prefix"; then
      read_failure=HIVE_SOCKET_UNAVAILABLE
    elif LC_ALL=C grep -Eq 'json: unsupported value: (NaN|[+-]?Inf)' "${work_dir}/read.stderr.prefix"; then
      read_failure=JSON_ENCODING_FAILED
    elif LC_ALL=C grep -Eqi 'unable to connect to the server|error dialing backend|error upgrading connection|i/o timeout|connection (reset|refused)|no route to host|tls handshake timeout|unexpected EOF' "${work_dir}/read.stderr.prefix"; then
      read_failure=TRANSPORT_FAILURE
    fi
    return 1
  fi
  if ! jq -e -s 'length==1 and .[0]!=null' "${destination}" >/dev/null 2>&1; then
    read_failure=INVALID_JSON
    return 1
  fi
}
roster() {
  local prefix="$1"
  read_json "${prefix}.ds" get daemonset cilium --namespace kube-system --output json || return 1
  read_json "${prefix}.pods" get pods --namespace kube-system --selector k8s-app=cilium --output json || return 1
  jq -e -S -c --slurpfile ds "${prefix}.ds" '
    $ds[0] as $d |
    [$d.spec.template.spec.containers[] | select(.name=="cilium-agent")] as $dc |
    if ($d.metadata.uid|type)!="string" or ($d.metadata.uid|length)==0 or
      ($dc|length)!=1 or
      ($dc[0].image|test("^quay.io/cilium/cilium:v1\\.20\\.3(@sha256:[a-f0-9]{64})?$"))!=true or
      ($d.status.desiredNumberScheduled|type)!="number" or
      $d.status.desiredNumberScheduled<1 or $d.status.desiredNumberScheduled>32 or
      $d.status.desiredNumberScheduled!=$d.status.numberAvailable or
      $d.status.desiredNumberScheduled!=$d.status.updatedNumberScheduled or
      $d.status.numberMisscheduled!=0 or $d.status.observedGeneration!=$d.metadata.generation or
      (.items|type)!="array" or (.items|length)!=$d.status.desiredNumberScheduled
    then error("incomplete daemonset") else . end |
    [.items[] |
      . as $p |
      [.spec.containers[]|select(.name=="cilium-agent")] as $c |
      [.status.containerStatuses[]|select(.name=="cilium-agent")] as $s |
      if (.metadata.name|test("^cilium-[a-z0-9]+$"))!=true or
        (.metadata.uid|type)!="string" or (.metadata.uid|length)==0 or
        .metadata.namespace!="kube-system" or .metadata.deletionTimestamp!=null or
        (.metadata.ownerReferences|map(select(.kind=="DaemonSet" and .name=="cilium" and
          .uid==$d.metadata.uid and .controller==true))|length)!=1 or
        .status.phase!="Running" or (.spec.nodeName|type)!="string" or (.spec.nodeName|length)==0 or
        ($c|length)!=1 or $c[0].image!=$dc[0].image or ($s|length)!=1 or $s[0].ready!=true or
        ($s[0].state.running|type)!="object" or ($s[0].restartCount|type)!="number" or
        ($s[0].containerID|type)!="string" or ($s[0].containerID|length)==0 or
        ($s[0].imageID|type)!="string" or ($s[0].imageID|length)==0
      then error("incomplete agent") else
        {name:$p.metadata.name,uid:$p.metadata.uid,node:$p.spec.nodeName,
         container:$s[0].containerID,image:$s[0].imageID,restarts:$s[0].restartCount}
      end] | sort_by(.name) |
    if (map(.name)|unique|length)!=length or (map(.node)|unique|length)!=length
    then error("duplicate agent") else {daemonset:$d.metadata.uid,generation:$d.metadata.generation,agents:.} end
  ' "${prefix}.pods" >"${prefix}.identity" 2>/dev/null
}

roster "${work_dir}/before" || inconclusive 'the ready agent roster could not be established.'
jq -r '.agents[].name' "${work_dir}/before.identity" >"${work_dir}/names"
: >"${work_dir}/safe"
for sample in 1 2 3 4 5; do
  [[ "${sample}" -eq 1 ]] || sleep 15
  agent=0
  while IFS= read -r pod; do
    agent=$((agent + 1))
    read_json "${work_dir}/metrics" exec --namespace kube-system "${pod}" --container cilium-agent \
      -- cilium-dbg metrics list --output json --match-pattern '^cilium_api_limiter_' ||
      inconclusive "an agent metric read failed (${read_failure})."
    captured_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if ! jq -e -c --argjson sample "${sample}" --argjson agent "${agent}" --arg time "${captured_at}" '
      def metric_key: .name+"/"+(.labels.value//"");
      ["cilium_api_limiter_processing_duration_seconds/mean",
       "cilium_api_limiter_processing_duration_seconds/estimated",
       "cilium_api_limiter_wait_duration_seconds/mean",
       "cilium_api_limiter_wait_duration_seconds/min",
       "cilium_api_limiter_wait_duration_seconds/max",
       "cilium_api_limiter_requests_in_flight/in-flight",
       "cilium_api_limiter_requests_in_flight/limit",
       "cilium_api_limiter_rate_limit/limit",
       "cilium_api_limiter_rate_limit/burst",
       "cilium_api_limiter_adjustment_factor/"] | sort as $expected |
      input |
      if type!="array" or length>10000 then error("invalid metrics") else . end |
      map(select(.labels.api_call=="endpoint-create")) as $endpoint |
      [$endpoint[]|select(.name!="cilium_api_limiter_processed_requests_total")|
        select(.name as $name | $expected|any(startswith($name+"/")))] as $gauges |
      [$endpoint[]|select(.name=="cilium_api_limiter_processed_requests_total")] as $counters |
      if ($gauges|map(metric_key)|sort)!=$expected or ($counters|length)==0 or
        (all(($gauges+$counters)[]; (.value|type)=="number" and .value>=0 and .value<=1e12))!=true or
        (all($gauges[]; (.labels|keys)==(if .name=="cilium_api_limiter_adjustment_factor"
          then ["api_call"] else ["api_call","value"] end)))!=true or
        (all($counters[]; (.labels|keys)==["api_call","outcome","return_code"]))!=true or
        ($counters|map(.labels)|unique|length)!=($counters|length)
      then error("incomplete metric set") else
        {sample:$sample,agent:$agent,capturedAt:$time,apiCall:"endpoint-create",
         metrics:([$gauges[]|{metric:.name,stat:(.labels.value//"current"),value:.value}]+
          [{metric:"cilium_api_limiter_processed_requests_total",stat:"all",value:($counters|map(.value)|add)}])}
      end
    ' --null-input "${work_dir}/metrics" >>"${work_dir}/safe" 2>"${work_dir}/parse.stderr"; then
      inconclusive 'a complete finite metric set could not be established.'
    fi
  done <"${work_dir}/names"
done
roster "${work_dir}/after" || inconclusive 'the final agent roster could not be established.'
cmp -s "${work_dir}/before.identity" "${work_dir}/after.identity" ||
  inconclusive 'agent identity or container state changed during collection.'
cat "${work_dir}/safe"
printf 'MEASURED: five complete samples per stable ready agent; rolling gauges only.\n'
