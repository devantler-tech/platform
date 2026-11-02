#!/usr/bin/env bash
# Prove that the Gateway refuses a connection whose physical peer is not the load balancer.
#
# THE QUESTION (devantler-tech/platform#5448, stage 3). Every TLS chain on the Gateway's listener
# accepts only the load balancer's private address. So far only allowed traffic has been observed,
# which cannot show the fence refuses anything. This must be answered before any PROXY change,
# because PROXY metadata names a client the listener must not take from an unbound peer.
#
# HOW. One short-lived pod connects straight to the gateway Service, so its physical peer is the
# pod and not the load balancer. A pod's connection is handled by the Envoy on its own node.
#   control  plain HTTP to an unrouted hostname. It must get the platform's HTTP-to-HTTPS redirect
#            (301) before and after the TLS attempts, which proves this pod reaches this Envoy and
#            that the plain chain, which carries no fence, still answers.
#   tls      HTTPS with the SNI of a public route. Certificate checks are off on purpose: a finished
#            handshake is the failure this looks for, whatever certificate came back.
#            refused   curl 35 or 56 with the TLS handshake never finished (time_appconnect is 0)
#            accepted  a finished handshake, any HTTP response or curl exit 0
#            other     anything else, such as a refused connect (7) or a timeout (28), which says
#                      the path failed and nothing about the fence
#
# WHY A REFUSAL IS ATTRIBUTABLE. Envoy counts a connection no chain matched in
# no_filter_chain_match. The script reads that counter before and after the attempts through the
# cilium-agent on the pod's node, with one fixed read-only command, because the Talos firewall closes
# Envoy's metrics port to the API server. Other traffic can only raise it, so a rise smaller than the
# attempts cannot be the fence refusing all of them, and is INCONCLUSIVE. A large enough rise supports
# the cause; it cannot single out this pod.
#
# THE VERDICT
#   REFUSED   both controls answered, every attempt was refused, and the counter rose by at least
#             the number of attempts.
#   ACCEPTED  at least one TLS attempt got an answer: the fence let this peer through.
#   INCONCLUSIVE  anything else.
#
# WHAT IT DOES NOT SHOW. It says nothing about forged PROXY metadata, original client identity or
# Email. Those stages remain open on #5448.
#
# WHAT IT WRITES. In `whoami`, one CiliumNetworkPolicy and one pod, both labelled with this run's
# id. No existing object is modified, and EXIT, INT and TERM delete everything carrying the label.
# A run refuses to start over labelled objects an earlier run left behind.
#
# WHAT IT PRINTS. The log of a public repository is public, so it prints only counts, curl exit
# codes and the verdict, never an address or node name. kubectl's stderr is discarded for that
# reason.
#
# EXIT CODES
#   0  REFUSED
#   1  usage error: nothing was read or written
#   3  INCONCLUSIVE: the run proved nothing and must not be reported as green
#   4  cleanup failed: delete the labelled objects before the next run
#   5  ACCEPTED: the fence did not refuse this peer
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly probe_namespace='whoami'
readonly probe_label='platform.devantler.tech/gateway-peer-probe'
readonly gateway_namespace='kube-system'
readonly gateway_service='cilium-gateway-platform'
readonly tls_host='platform.devantler.tech'
readonly control_host='unrouted.gateway-peer-probe.invalid'
# The same pinned image the cross-node probe and the coroot heartbeat CronJob run.
readonly client_image='docker.io/curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777'
readonly warmup_attempts=12
readonly attempt_timeout=5
# Time the script has to read the first counter value once the pod reports it is ready. A slower
# read can only make the rise look smaller, which reads INCONCLUSIVE, never REFUSED.
readonly settle_seconds=20
readonly poll_seconds="${PROBE_POLL_SECONDS:-2}"

usage() {
  printf 'Usage: %s --context <kube-context> --run-id <digits> [--attempts N]\n' "$(basename "$0")" >&2
}

context=''
run_id=''
attempts='10'
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context | --run-id | --attempts)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage
        exit 1
      fi
      case "$1" in
        --context) context="$2" ;;
        --run-id) run_id="$2" ;;
        --attempts) attempts="$2" ;;
      esac
      shift 2
      ;;
    *)
      usage
      exit 1
      ;;
  esac
done
if [[ -z "${context}" ]] || [[ ! "${run_id}" =~ ^[1-9][0-9]{0,19}$ ]]; then
  usage
  exit 1
fi
if [[ ! "${attempts}" =~ ^[1-9][0-9]?$ ]] || ((attempts > 30)); then
  printf 'Refusing to run: --attempts must be a whole number from 1 to 30.\n' >&2
  exit 1
fi

# Worst case: warm-up, the settle wait, every attempt timing out, the closing control, and pull.
readonly pod_deadline_seconds=$(((warmup_attempts + attempts + 1) * attempt_timeout + warmup_attempts + settle_seconds + 120))
readonly probe_pod="gateway-peer-probe-${run_id}"
readonly probe_policy="gateway-peer-probe-${run_id}"

summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >>"${GITHUB_STEP_SUMMARY}" || true
  fi
}

finish() {
  printf 'VERDICT: %s\n' "$1"
  printf 'Reason: %s\n' "$2"
  summary "### Gateway denied-peer probe (#5448)"
  summary ""
  summary "**Verdict:** $1: $2"
  exit "$3"
}

inconclusive() {
  finish INCONCLUSIVE "$1" 3
}

# Every request is bounded, so a stalled API server ends in INCONCLUSIVE, never in a hang.
kc() {
  kubectl --context "${context}" --request-timeout=30s "$@" 2>/dev/null
}

leftovers="$(kc -n "${probe_namespace}" get pods,ciliumnetworkpolicies -l "${probe_label}" -o name)" ||
  inconclusive 'could not read for objects an earlier run left behind'
if [[ -n "${leftovers}" ]]; then
  inconclusive 'objects from an earlier run remain; nothing was created'
fi
gateway_ip="$(kc -n "${gateway_namespace}" get service "${gateway_service}" -o jsonpath='{.spec.clusterIP}')" ||
  inconclusive 'could not read the gateway Service'
if [[ ! "${gateway_ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  inconclusive 'the gateway Service has no IPv4 cluster address'
fi

# Invoked by the EXIT trap below. CI's shellcheck reports that as SC2317, newer releases as SC2329.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if ! kc -n "${probe_namespace}" delete pods,ciliumnetworkpolicies -l "${probe_label}=${run_id}" --ignore-not-found --wait=true --timeout=25s >/dev/null; then
    printf 'CLEANUP: FAILED, delete objects labelled %s=%s by hand\n' "${probe_label}" "${run_id}"
    summary ""
    summary "**Cleanup failed:** delete objects labelled \`${probe_label}=${run_id}\` by hand."
    exit 4
  fi
  printf 'CLEANUP: done\n'
  exit "${rc}"
}
trap cleanup EXIT
# A cancelled job sends TERM (or INT); turning it into a normal exit runs the same cleanup at once.
trap 'exit 143' INT TERM

manifest() {
  cat <<YAML
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: ${probe_policy}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  endpointSelector:
    matchLabels:
      ${probe_label}: "${run_id}"
  egress:
    - toServices:
        - k8sService:
            serviceName: ${gateway_service}
            namespace: ${gateway_namespace}
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
            - port: "443"
              protocol: TCP
    - toEntities:
        - ingress
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
            - port: "443"
              protocol: TCP
---
apiVersion: v1
kind: Pod
metadata:
  name: ${probe_pod}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  restartPolicy: Never
  activeDeadlineSeconds: ${pod_deadline_seconds}
  terminationGracePeriodSeconds: 5
  automountServiceAccountToken: false
  enableServiceLinks: false
  hostUsers: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: probe
      image: ${client_image}
      env:
        - name: ATTEMPTS
          value: "${attempts}"
        - name: WARMUP_ATTEMPTS
          value: "${warmup_attempts}"
        - name: TIMEOUT
          value: "${attempt_timeout}"
        - name: SETTLE_SECONDS
          value: "${settle_seconds}"
        - name: GATEWAY_IP
          value: "${gateway_ip}"
        - name: TLS_HOST
          value: ${tls_host}
        - name: CONTROL_HOST
          value: ${control_host}
      command:
        - /bin/sh
        - -c
        - |
          set -u
          control() {
            curl -s -o /dev/null --max-time "\$TIMEOUT" -H "Host: \$CONTROL_HOST" -w '%{http_code}' "http://\$GATEWAY_IP/" 2>/dev/null || true
          }
          i=0
          first=''
          while [ "\$i" -lt "\$WARMUP_ATTEMPTS" ]; do
            i=\$((i + 1))
            first="\$(control)"
            [ "\$first" = 301 ] && break
            sleep 1
          done
          printf 'PROBE control-before %s\\n' "\$first"
          printf 'PROBE-READY\\n'
          sleep "\$SETTLE_SECONDS"
          i=0
          while [ "\$i" -lt "\$ATTEMPTS" ]; do
            i=\$((i + 1))
            code="\$(curl -sk -o /dev/null --max-time "\$TIMEOUT" --resolve "\$TLS_HOST:443:\$GATEWAY_IP" -w '%{http_code} %{time_appconnect}' "https://\$TLS_HOST/" 2>/dev/null)"
            printf 'PROBE tls %s %s\\n' "\$?" "\$code"
          done
          printf 'PROBE control-after %s\\n' "\$(control)"
          printf 'PROBE-DONE\\n'
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        capabilities:
          drop:
            - ALL
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          cpu: 100m
          memory: 64Mi
YAML
}

manifest | kc apply -f - >/dev/null || inconclusive 'the probe objects were not admitted'
printf 'Created one pod and one policy, labelled with run %s.\n' "${run_id}"

pod_logs() {
  kc -n "${probe_namespace}" logs "${probe_pod}" || true
}

# Prints the sum of the node's Envoy no_filter_chain_match series, or nothing when the read fails or
# the counter is not there: a missing series is not a zero.
read_counter() {
  local metrics
  metrics="$(kc -n "${gateway_namespace}" exec "${agent_pod}" --container cilium-agent -- cilium-dbg envoy admin metrics --filter no_filter_chain_match)" || return 0
  grep -E '^[^#]*no_filter_chain_match' <<<"${metrics}" | awk '{ sum += $NF; seen = 1 } END { if (seen) printf "%d", sum }' || true
}

# Waits for the pod to print the marker, within the pod's own deadline.
wait_for_marker() {
  local marker="$1" deadline
  deadline=$((SECONDS + pod_deadline_seconds + 60))
  while ((SECONDS < deadline)); do
    if grep -q "^${marker}\$" <<<"$(pod_logs)"; then
      return 0
    fi
    sleep "${poll_seconds}"
  done
  return 1
}

wait_for_marker 'PROBE-READY' || inconclusive 'the pod did not finish its control requests in time'
node="$(kc -n "${probe_namespace}" get pod "${probe_pod}" -o jsonpath='{.spec.nodeName}')" || node=''
[[ -n "${node}" ]] || inconclusive 'could not read which node holds the pod'
agent_pod="$(kc -n "${gateway_namespace}" get pods -l k8s-app=cilium --field-selector "spec.nodeName=${node}" -o jsonpath='{.items[0].metadata.name}')" || agent_pod=''
[[ -n "${agent_pod}" ]] || inconclusive 'could not find the Cilium agent on the node that holds the pod'
counter_before="$(read_counter)"

wait_for_marker 'PROBE-DONE' || inconclusive 'the pod did not finish its attempts in time'
counter_after="$(read_counter)"

logs="$(pod_logs)"
control_before="$(awk '$2 == "control-before" { print $3 }' <<<"${logs}")"
control_after="$(awk '$2 == "control-after" { print $3 }' <<<"${logs}")"
# A line is `PROBE tls <curl exit> <http code> <time_appconnect>`. curl prints a zero time for a
# handshake that never finished, and any other value means this peer completed one.
counted="$(awk '$1 == "PROBE" && $2 == "tls" { n++ } END { print n + 0 }' <<<"${logs}")"
accepted="$(awk '$1 == "PROBE" && $2 == "tls" && ($3 == 0 || ($4 != "000" && $4 != "") || $5 + 0 > 0) { n++ } END { print n + 0 }' <<<"${logs}")"
refused="$(awk '$1 == "PROBE" && $2 == "tls" && ($3 == 35 || $3 == 56) && $4 == "000" && $5 != "" && $5 + 0 == 0 { n++ } END { print n + 0 }' <<<"${logs}")"
other=$((counted - refused - accepted))

printf 'Controls before and after: %s %s\n' "${control_before:-none}" "${control_after:-none}"
printf 'TLS attempts: %s of %s made, refused=%s accepted=%s other=%s\n' "${counted}" "${attempts}" "${refused}" "${accepted}" "${other}"
printf 'curl exits: %s\n' "$(awk '$1 == "PROBE" && $2 == "tls" { print $3 }' <<<"${logs}" | sort -n | uniq -c | awk '{ printf "%s×%s ", $2, $1 }')"

if ((accepted > 0)); then
  finish ACCEPTED "${accepted} of ${counted} TLS attempts from a peer that is not the load balancer got an answer" 5
fi
if [[ "${control_before}" != 301 || "${control_after}" != 301 ]]; then
  inconclusive 'the plain HTTP control did not answer 301 before and after, so the path is not established'
fi
if ((counted != attempts || refused != attempts)); then
  inconclusive "only ${refused} of ${attempts} attempts ended as a closed connection"
fi
if [[ -z "${counter_before}" || -z "${counter_after}" ]]; then
  inconclusive 'every attempt was closed, but the Envoy counter could not be read, so the fence is not shown as the cause'
fi
rise=$((counter_after - counter_before))
printf 'Envoy no_filter_chain_match rise on the pod node: %s\n' "${rise}"
if ((rise < attempts)); then
  inconclusive "every attempt was closed, but the Envoy counter rose by only ${rise}"
fi
finish REFUSED "all ${attempts} TLS attempts from a pod were closed with no matching chain, and the plain HTTP control still answered" 0
