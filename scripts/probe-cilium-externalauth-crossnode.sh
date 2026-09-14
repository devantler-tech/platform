#!/usr/bin/env bash
# Measure whether the Gateway API ExternalAuth subrequest crosses nodes on the deployed Cilium.
#
# THE QUESTION (devantler-tech/platform#2284, #3784). On Cilium 1.20.0-pre.3 the gateway Envoy's
# ext_authz subrequest to oauth2-proxy was black-holed whenever it crossed nodes. The ipcache read
# (diagnose-cilium-ext-authz-ipcache.sh) cannot settle it while node encryption is off, so this
# script sends real requests through a test-only route and observes whether they arrive.
#
# WHY THE PATH IS KNOWN, NOT GUESSED. Cilium runs one Envoy per node, and pod socket load balancing
# is off, so a pod's connection to the gateway Service is handled by the Envoy on the POD'S OWN node.
# That Envoy makes the ExternalAuth call. So:
#   * a client pinned to a node running NO oauth2-proxy replica forces every call to cross nodes;
#   * a client pinned to a node running a replica is the same-node control (Envoy does not prefer
#     local endpoints, so it is a mix, which is enough to prove ext_authz works at all).
# Replica placement and the node set are read before and after; any change is INCONCLUSIVE.
#
# WHY A FAILURE IS ATTRIBUTABLE. Requests stay inside the cluster, over plain HTTP on the gateway's
# `http` listener, to hostnames under `externalauth-probe.invalid`. Nothing in the path is
# Cloudflare, public DNS or TLS, and external-dns publishes nothing outside the zone. Each client
# alternates two routes with the SAME backend: a plain control route and the ExternalAuth route.
# If the control route does not answer every time, the path or policy is broken and the run is
# INCONCLUSIVE rather than blamed on ext_authz.
#
#   delivered  a 302/303 whose Location is the Dex login (only oauth2-proxy produces it), or a 401
#   lost       no response within the timeout, a 5xx, or an EMPTY 403 (Envoy's ext_authz error)
#   other      anything else (a 200 means the filter is not applied; a 404 means the route is not
#              programmed) — counted, and any occurrence makes the run INCONCLUSIVE
#
# THE VERDICT
#   FIXED           control routes answered every time, the same-node client was delivered at least
#                   once, and EVERY cross-node request was delivered.
#   FAULT-PERSISTS  control routes answered every time, the same-node client was delivered at least
#                   once, and at least one cross-node request was lost.
#   INCONCLUSIVE    anything else.
#
# WHAT IT WRITES — stated plainly, because unlike the ipcache read this changes production while it
# runs. Everything carries the label `platform.devantler.tech/externalauth-probe=<run-id>`:
#   * in `whoami`: two HTTPRoutes (control, ExternalAuth), one CiliumNetworkPolicy letting the two
#     probe pods reach the gateway Service, and two pods;
#   * in `oauth2-proxy`: one ReferenceGrant letting the ExternalAuth route name the oauth2-proxy
#     Service.
# No existing object is modified. An EXIT trap deletes everything carrying the run's label. If a
# previous run left labelled objects behind, this run refuses before creating anything.
#
# WHAT IT PRINTS. The workflow log of a public repository is public, so no address, node name, UID
# or redirect URL is printed — only counts and the verdict. kubectl's stderr is discarded for the
# same reason.
#
# EXIT CODES
#   0  a conclusive verdict (FIXED or FAULT-PERSISTS)
#   1  usage error — nothing was read or written
#   3  INCONCLUSIVE — the run proved nothing, and a caller must not report it as green
#   4  cleanup failed — labelled objects may remain; delete them by label before the next run
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly probe_namespace='whoami'
readonly oauth2_namespace='oauth2-proxy'
readonly oauth2_selector='app.kubernetes.io/name=oauth2-proxy,app.kubernetes.io/instance=oauth2-proxy'
readonly probe_label='platform.devantler.tech/externalauth-probe'
readonly control_host='control.externalauth-probe.invalid'
readonly authz_host='authz.externalauth-probe.invalid'
readonly gateway_url='http://cilium-gateway-platform.kube-system.svc.cluster.local/'
# The same pinned image the coroot heartbeat CronJob runs; it is not first-party, so no Talos image
# verification rule matches it.
readonly client_image='docker.io/curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777'
# Each pod sends 2 requests per round (control + ExternalAuth), each bounded by the timeout, so its
# worst case is 2 * requests * timeout. The cap keeps that, plus warm-up and scheduling, well
# inside the workflow's job timeout (pinned by the test).
readonly max_request_seconds=250
readonly warmup_attempts=30
readonly route_wait_attempts=24
readonly poll_seconds="${PROBE_POLL_SECONDS:-5}"

usage() {
  printf 'Usage: %s --context <kube-context> --run-id <digits> [--requests N] [--timeout SECONDS]\n' "$(basename "$0")" >&2
}

context=''
run_id=''
requests='40'
timeout='5'
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context | --run-id | --requests | --timeout)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage
        exit 1
      fi
      case "$1" in
        --context) context="$2" ;;
        --run-id) run_id="$2" ;;
        --requests) requests="$2" ;;
        --timeout) timeout="$2" ;;
      esac
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown argument: %s\n' "$1" >&2
      usage
      exit 1
      ;;
  esac
done

# Required, never defaulted: writing to whatever the kubeconfig's current-context happens to be would
# change a cluster that is not the one being measured.
if [[ -z "${context}" ]]; then
  printf 'Refusing to run: --context is required.\n' >&2
  usage
  exit 1
fi
# The run id becomes part of object names and a label value, so it is digits only (a GitHub run id).
if [[ ! "${run_id}" =~ ^[1-9][0-9]{0,19}$ ]]; then
  printf 'Refusing to run: --run-id must be a decimal number with no leading zero.\n' >&2
  usage
  exit 1
fi
# Plain decimal with no leading zero BEFORE any arithmetic: bash reads `08` as invalid octal.
if [[ ! "${requests}" =~ ^[1-9][0-9]{0,2}$ ]] || ((requests > 100)); then
  printf 'Refusing to run: --requests must be a whole number from 1 to 100.\n' >&2
  usage
  exit 1
fi
if [[ ! "${timeout}" =~ ^[1-9][0-9]?$ ]] || ((timeout > 10)); then
  printf 'Refusing to run: --timeout must be a whole number of seconds from 1 to 10.\n' >&2
  usage
  exit 1
fi
if ((requests * timeout > max_request_seconds)); then
  printf 'Refusing to run: --requests x --timeout must not exceed %s seconds.\n' "${max_request_seconds}" >&2
  usage
  exit 1
fi

readonly pod_deadline_seconds=$((2 * requests * timeout + 2 * warmup_attempts + 60))
readonly pod_wait_seconds=$((pod_deadline_seconds + 180))
readonly cross_pod="externalauth-probe-cross-${run_id}"
readonly same_pod="externalauth-probe-same-${run_id}"
readonly control_route="externalauth-probe-control-${run_id}"
readonly authz_route="externalauth-probe-authz-${run_id}"
readonly probe_policy="externalauth-probe-client-${run_id}"
readonly probe_grant="externalauth-probe-${run_id}"

summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >>"${GITHUB_STEP_SUMMARY}" || true
  fi
}

inconclusive() {
  printf 'VERDICT: INCONCLUSIVE\n'
  printf 'Reason: %s\n' "$1"
  summary "### Cilium ExternalAuth cross-node probe (#2284)"
  summary ""
  summary "**Verdict:** INCONCLUSIVE — $1"
  exit 3
}

conclude() {
  printf 'VERDICT: %s\n' "$1"
  printf 'Reason: %s\n' "$2"
  summary "### Cilium ExternalAuth cross-node probe (#2284)"
  summary ""
  summary "**Verdict:** $1 — $2"
  exit 0
}

kc() {
  kubectl --context "${context}" "$@" 2>/dev/null
}

is_k8s_name() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]
}

# Shared jq definitions so the before and after reads apply the SAME tests.
readonly replica_jq_defs='
  def ready: ([.status.conditions[]? | select(.type == "Ready") | .status] == ["True"]);
  def replicas: [.items[]? | {
      ok: (.status.phase == "Running" and .metadata.deletionTimestamp == null and ready),
      uid: (.metadata.uid // ""),
      node: (.spec.nodeName // "")
    }];
  def fingerprint: map(.uid + "|" + .node) | sort | join(";");
'
# A node can run a probe pod when it is Ready and carries no NoSchedule/NoExecute taint.
readonly node_jq_defs='
  def ready: ([.status.conditions[]? | select(.type == "Ready") | .status] == ["True"]);
  def schedulable: (.spec.unschedulable != true)
    and ([.spec.taints[]? | select(.effect == "NoSchedule" or .effect == "NoExecute")] | length == 0);
  def fingerprint: [.items[]? | (.metadata.name // "") + "|" + (.metadata.uid // "")] | sort | join(";");
'

read_replicas() {
  local json
  json="$(kc -n "${oauth2_namespace}" get pods -l "${oauth2_selector}" -o json)" || return 1
  jq -r "${replica_jq_defs}"'
    replicas
    | if length == 0 then "ERR none"
      elif any(.[]; (.ok | not) or .uid == "" or .node == "") then "ERR unsettled"
      else "FP " + fingerprint, (.[] | "NODE " + .node)
      end
  ' <<<"${json}"
}

read_nodes() {
  local json
  json="$(kc get nodes -o json)" || return 1
  jq -r "${node_jq_defs}"'
    if ([.items[]?] | length) == 0 then "ERR none"
    elif any(.items[]?; (.metadata.name // "") == "" or (.metadata.uid // "") == "") then "ERR identity"
    else "FP " + fingerprint,
      ([.items[] | select(ready and schedulable) | .metadata.name] | sort | .[] | "SCHED " + .)
    end
  ' <<<"${json}"
}

# ---------------------------------------------------------------------------
# 1. Topology before: settled oauth2-proxy replicas, and the nodes a probe pod may run on.
# ---------------------------------------------------------------------------
replica_lines="$(read_replicas)" || inconclusive 'could not read the oauth2-proxy pods'
case "${replica_lines}" in
  'ERR none') inconclusive 'no oauth2-proxy pods were found' ;;
  'ERR unsettled') inconclusive 'an oauth2-proxy replica is not settled (rollout in flight)' ;;
esac
replica_fp_before="$(sed -n 's/^FP //p' <<<"${replica_lines}")"
replica_nodes="$(sed -n 's/^NODE //p' <<<"${replica_lines}" | sort -u)"

node_lines="$(read_nodes)" || inconclusive 'could not read the nodes'
case "${node_lines}" in
  'ERR none') inconclusive 'no nodes were found' ;;
  'ERR identity') inconclusive 'a node reported no name or UID' ;;
esac
node_fp_before="$(sed -n 's/^FP //p' <<<"${node_lines}")"
if [[ -z "${replica_fp_before}" || -z "${node_fp_before}" ]]; then
  inconclusive 'the topology reads did not parse'
fi

cross_node=''
same_node=''
while IFS= read -r node; do
  [[ -z "${node}" ]] && continue
  is_k8s_name "${node}" || inconclusive 'a node reported a malformed name'
  if grep -Fxq -- "${node}" <<<"${replica_nodes}"; then
    [[ -z "${same_node}" ]] && same_node="${node}"
  else
    [[ -z "${cross_node}" ]] && cross_node="${node}"
  fi
done < <(sed -n 's/^SCHED //p' <<<"${node_lines}")

[[ -n "${cross_node}" ]] || inconclusive 'no schedulable node without an oauth2-proxy replica exists, so no cross-node path can be forced'
[[ -n "${same_node}" ]] || inconclusive 'no schedulable node hosts an oauth2-proxy replica, so there is no same-node control'

printf 'oauth2-proxy replicas on %s node(s); schedulable nodes: %s\n' \
  "$(grep -c . <<<"${replica_nodes}" || true)" "$(grep -c '^SCHED ' <<<"${node_lines}" || true)"

# ---------------------------------------------------------------------------
# 2. Refuse to run over leftovers. A labelled object from an earlier run means its cleanup failed;
#    this run must not measure through it, and must not delete it (it is not this run's).
# ---------------------------------------------------------------------------
if ! leftovers="$(kc -n "${probe_namespace}" get httproutes,ciliumnetworkpolicies,pods -l "${probe_label}" -o name)"; then
  inconclusive 'could not check for objects left by an earlier probe run'
fi
if ! leftover_grants="$(kc -n "${oauth2_namespace}" get referencegrants -l "${probe_label}" -o name)"; then
  inconclusive 'could not check for objects left by an earlier probe run'
fi
if [[ -n "${leftovers}${leftover_grants}" ]]; then
  inconclusive "objects labelled ${probe_label} already exist; delete them by label before running again"
fi

# ---------------------------------------------------------------------------
# 3. Create the probe objects. From here on, the EXIT trap removes everything this run labelled.
# ---------------------------------------------------------------------------
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  local rc=$?
  local failed=0
  kc -n "${probe_namespace}" delete pods,httproutes,ciliumnetworkpolicies -l "${probe_label}=${run_id}" --ignore-not-found --wait=false >/dev/null || failed=1
  kc -n "${oauth2_namespace}" delete referencegrants -l "${probe_label}=${run_id}" --ignore-not-found --wait=false >/dev/null || failed=1
  if [[ "${failed}" -ne 0 ]]; then
    printf 'CLEANUP: FAILED — delete objects labelled %s=%s by hand\n' "${probe_label}" "${run_id}"
    summary ""
    summary "**Cleanup failed** — delete objects labelled \`${probe_label}=${run_id}\` by hand."
    exit 4
  fi
  printf 'CLEANUP: done\n'
  exit "${rc}"
}
trap cleanup EXIT

route_manifest() {
  local name="$1" host="$2" filters="$3"
  cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ${name}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  parentRefs:
    - name: platform
      namespace: kube-system
      sectionName: http
  hostnames:
    - ${host}
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
${filters}
      backendRefs:
        - name: whoami
          port: 80
YAML
}

# The same ExternalAuth filter shape the reverted cutover used (#2283), so the probe exercises the
# datapath #1881 would ship.
readonly authz_filters='      filters:
        - type: ExternalAuth
          externalAuth:
            protocol: HTTP
            backendRef:
              name: oauth2-proxy
              namespace: oauth2-proxy
              port: 80
            http:
              allowedHeaders:
                - cookie
                - x-forwarded-proto
                - x-forwarded-host
              allowedResponseHeaders:
                - set-cookie
                - x-auth-request-user
                - x-auth-request-email
                - x-auth-request-groups'

pod_manifest() {
  local name="$1" node="$2" role="$3"
  cat <<YAML
---
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${probe_namespace}
  labels:
    ${probe_label}: "${run_id}"
    platform.devantler.tech/externalauth-probe-role: ${role}
spec:
  restartPolicy: Never
  activeDeadlineSeconds: ${pod_deadline_seconds}
  automountServiceAccountToken: false
  enableServiceLinks: false
  hostUsers: false
  nodeSelector:
    kubernetes.io/hostname: ${node}
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
        - name: REQUESTS
          value: "${requests}"
        - name: TIMEOUT
          value: "${timeout}"
        - name: WARMUP_ATTEMPTS
          value: "${warmup_attempts}"
        - name: CONTROL_HOST
          value: ${control_host}
        - name: AUTHZ_HOST
          value: ${authz_host}
        - name: GATEWAY_URL
          value: ${gateway_url}
      command:
        - /bin/sh
        - -c
        - |
          set -u
          ask() {
            curl -s -o /dev/null --max-time "\$TIMEOUT" -H "Host: \$1" -w '%{http_code} %{size_download} %{redirect_url}' "\$GATEWAY_URL" 2>/dev/null
          }
          # Warm-up: both routes must be programmed before anything is counted. A 404 means the
          # gateway has not picked the route up yet; any other answer on both routes means it has.
          warm=0
          n=0
          while [ "\$n" -lt "\$WARMUP_ATTEMPTS" ]; do
            n=\$((n + 1))
            c=\$(ask "\$CONTROL_HOST" | cut -d' ' -f1)
            a=\$(ask "\$AUTHZ_HOST" | cut -d' ' -f1)
            if [ "\$c" = 200 ] && [ -n "\$a" ] && [ "\$a" != 000 ] && [ "\$a" != 404 ]; then
              warm=1
              break
            fi
            sleep 1
          done
          if [ "\$warm" -ne 1 ]; then
            printf 'PROBE-WARMUP failed\\n'
            exit 0
          fi
          printf 'PROBE-WARMUP ok\\n'
          i=0
          while [ "\$i" -lt "\$REQUESTS" ]; do
            i=\$((i + 1))
            printf 'PROBE control %s\\n' "\$(ask "\$CONTROL_HOST" || true)"
            printf 'PROBE authz %s\\n' "\$(ask "\$AUTHZ_HOST" || true)"
          done
          printf 'PROBE-DONE %s\\n' "\$i"
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

manifest="$(
  cat <<YAML
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: ${probe_grant}
  namespace: ${oauth2_namespace}
  labels:
    ${probe_label}: "${run_id}"
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: ${probe_namespace}
  to:
    - group: ""
      kind: Service
      name: oauth2-proxy
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
            serviceName: cilium-gateway-platform
            namespace: kube-system
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
    - toEntities:
        - ingress
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
YAML
  route_manifest "${control_route}" "${control_host}" ''
  route_manifest "${authz_route}" "${authz_host}" "${authz_filters}"
)"

if ! kc apply -f - <<<"${manifest}" >/dev/null; then
  inconclusive 'could not create the probe routes, grant and policy'
fi

# Both routes must be Accepted with resolved references before a client is started, so a slow
# controller is not measured as a lost subrequest.
routes_ready=0
attempt=0
while [[ "${attempt}" -lt "${route_wait_attempts}" ]]; do
  attempt=$((attempt + 1))
  if routes_json="$(kc -n "${probe_namespace}" get httproutes -l "${probe_label}=${run_id}" -o json)"; then
    ready_count="$(jq -r '
      def cond($t): [.status.parents[]?.conditions[]? | select(.type == $t) | .status];
      [.items[]? | select((cond("Accepted") | length) > 0 and all(cond("Accepted")[]; . == "True")
                          and (cond("ResolvedRefs") | length) > 0 and all(cond("ResolvedRefs")[]; . == "True"))]
      | length' <<<"${routes_json}" 2>/dev/null || printf '0')"
    if [[ "${ready_count}" == '2' ]]; then
      routes_ready=1
      break
    fi
  fi
  sleep "${poll_seconds}"
done
[[ "${routes_ready}" -eq 1 ]] || inconclusive 'the probe routes were not accepted with resolved references in time'

pods_manifest="$(
  pod_manifest "${cross_pod}" "${cross_node}" cross
  pod_manifest "${same_pod}" "${same_node}" same
)"
if ! kc apply -f - <<<"${pods_manifest}" >/dev/null; then
  inconclusive 'could not create the probe pods'
fi

# ---------------------------------------------------------------------------
# 4. Wait for both clients to finish, on the nodes they were pinned to.
# ---------------------------------------------------------------------------
wait_pod() {
  local name="$1" node="$2" waited=0 json phase actual
  while [[ "${waited}" -lt "${pod_wait_seconds}" ]]; do
    if json="$(kc -n "${probe_namespace}" get pod "${name}" -o json)"; then
      phase="$(jq -r '.status.phase // ""' <<<"${json}")"
      actual="$(jq -r '.spec.nodeName // ""' <<<"${json}")"
      case "${phase}" in
        Succeeded)
          [[ "${actual}" == "${node}" ]] || return 2
          return 0
          ;;
        Failed) return 1 ;;
      esac
    fi
    sleep "${poll_seconds}"
    # At least one second per iteration, so a zero poll interval (the test) still terminates.
    waited=$((waited + (poll_seconds > 0 ? poll_seconds : 1)))
  done
  return 1
}

for pair in "${cross_pod}:${cross_node}" "${same_pod}:${same_node}"; do
  name="${pair%%:*}"
  node="${pair#*:}"
  set +e
  wait_pod "${name}" "${node}"
  status=$?
  set -e
  case "${status}" in
    0) ;;
    2) inconclusive 'a probe pod ran on a node other than the one it was pinned to' ;;
    *) inconclusive 'a probe pod did not complete (not scheduled, refused, or past its deadline)' ;;
  esac
done

# ---------------------------------------------------------------------------
# 5. Classify each client's answers.
# ---------------------------------------------------------------------------
# Prints: control_ok control_bad delivered lost other
classify() {
  local log="$1"
  awk -v want="${requests}" '
    $1 == "PROBE-WARMUP" { warm = $2 }
    $1 == "PROBE-DONE" { done = $2 }
    $1 == "PROBE" && $2 == "control" {
      nc++
      if ($3 == "200") ok++; else bad++
    }
    $1 == "PROBE" && $2 == "authz" {
      na++
      code = $3; size = $4; loc = $5
      if ((code == "302" || code == "303") && index(loc, "https://dex.") == 1) delivered++
      else if (code == "401") delivered++
      else if (code == "000" || code == "" || code ~ /^5[0-9][0-9]$/ || (code == "403" && size == "0")) lost++
      else other++
    }
    END {
      if (warm != "ok") { print "ERR warmup"; exit }
      if (done != want || nc != want || na != want) { print "ERR incomplete"; exit }
      printf "%d %d %d %d %d\n", ok, bad, delivered, lost, other
    }
  ' <<<"${log}"
}

results=''
for role in cross same; do
  if [[ "${role}" == 'cross' ]]; then name="${cross_pod}"; else name="${same_pod}"; fi
  log="$(kc -n "${probe_namespace}" logs "${name}")" || inconclusive 'could not read a probe pod log'
  counts="$(classify "${log}")"
  case "${counts}" in
    'ERR warmup') inconclusive 'a probe pod could not reach both routes during warm-up, so the path or policy is broken' ;;
    'ERR incomplete') inconclusive 'a probe pod log did not contain every expected answer' ;;
  esac
  results="${results}${role} ${counts}"$'\n'
done

# ---------------------------------------------------------------------------
# 6. Topology after: the placement the verdict relies on must not have changed.
# ---------------------------------------------------------------------------
replica_lines_after="$(read_replicas)" || inconclusive 'could not re-read the oauth2-proxy pods'
[[ "$(sed -n 's/^FP //p' <<<"${replica_lines_after}")" == "${replica_fp_before}" ]] ||
  inconclusive 'the oauth2-proxy replicas changed during the run'
node_lines_after="$(read_nodes)" || inconclusive 'could not re-read the nodes'
[[ "$(sed -n 's/^FP //p' <<<"${node_lines_after}")" == "${node_fp_before}" ]] ||
  inconclusive 'the node set changed during the run'

# ---------------------------------------------------------------------------
# 7. Verdict.
# ---------------------------------------------------------------------------
read -r _ cross_ok cross_bad cross_delivered cross_lost cross_other <<<"$(grep '^cross ' <<<"${results}")"
read -r _ same_ok same_bad same_delivered same_lost same_other <<<"$(grep '^same ' <<<"${results}")"

printf 'cross-node client: control %s ok / %s failed; ExternalAuth %s delivered / %s lost / %s other\n' \
  "${cross_ok}" "${cross_bad}" "${cross_delivered}" "${cross_lost}" "${cross_other}"
printf 'same-node client:  control %s ok / %s failed; ExternalAuth %s delivered / %s lost / %s other\n' \
  "${same_ok}" "${same_bad}" "${same_delivered}" "${same_lost}" "${same_other}"
summary "| client | control ok | control failed | delivered | lost | other |"
summary "| --- | --- | --- | --- | --- | --- |"
summary "| cross-node | ${cross_ok} | ${cross_bad} | ${cross_delivered} | ${cross_lost} | ${cross_other} |"
summary "| same-node | ${same_ok} | ${same_bad} | ${same_delivered} | ${same_lost} | ${same_other} |"
summary ""

if ((cross_bad + same_bad > 0)); then
  inconclusive 'the plain control route did not answer every time, so failures cannot be attributed to ExternalAuth'
fi
if ((cross_other + same_other > 0)); then
  inconclusive 'the ExternalAuth route returned answers that are neither delivered nor lost (filter not applied, or route not serving)'
fi
if ((same_delivered == 0)); then
  inconclusive 'no ExternalAuth request was delivered even from a node with a local replica, so the route or backend is broken'
fi
if ((cross_lost == 0)); then
  if ((same_lost > 0)); then
    inconclusive 'cross-node requests were all delivered but same-node requests were lost, which is not the #2284 pattern'
  fi
  conclude 'FIXED' "every cross-node ExternalAuth request (${cross_delivered}) was delivered"
fi
conclude 'FAULT-PERSISTS' "${cross_lost} of ${requests} cross-node ExternalAuth requests were lost while the control route answered every time"
