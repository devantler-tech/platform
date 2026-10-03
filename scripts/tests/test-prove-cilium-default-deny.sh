#!/usr/bin/env bash
# Offline safety and evidence regressions. Only Kubernetes/Docker process calls
# are replaced; the runner, generation, JSON, policy parsing and files are real.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
runner="${root}/scripts/prove-cilium-default-deny.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir "${work}/bin"
cat >"${work}/bin/ksail" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "ksail $*" >>"${FAKE_STATE}/commands"
if [[ "$*" == *'--version'* ]]; then
  if [[ "${FAKE_CASE}" == wrong_ksail ]]; then echo 'ksail version 7.193.80'; else echo 'ksail version 7.193.8'; fi
  exit
fi
if [[ "$*" == *'cluster create'* ]]; then touch "${FAKE_STATE}/created"; exit; fi
if [[ "$*" == *'cluster delete'* ]]; then
  [[ "${FAKE_CASE}" != cleanup_error ]] || exit 1
  rm -f "${FAKE_STATE}/created"
  exit
fi
exit 90
STUB
cat >"${work}/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "docker $*" >>"${FAKE_STATE}/commands"
[[ "$1" == ps ]] || exit 90
[[ "${FAKE_CASE}" != docker_error ]] || exit 1
if [[ "${FAKE_CASE}" == occupied && "$*" != *--filter* ]]; then echo unrelated-container; fi
if [[ -f "${FAKE_STATE}/created" ]]; then echo proof-node; fi
STUB
cat >"${work}/bin/timeout" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
shift
exec "$@"
STUB
cat >"${work}/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat >"${work}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "kubectl $*" >>"${FAKE_STATE}/commands"
args="$*"
if [[ "$args" == *'apply -f'* ]]; then
  [[ "$args" != *allows.yaml* ]] || touch "${FAKE_STATE}/helper"
  [[ "$args" != *runtime-policies.yaml* ]] || touch "${FAKE_STATE}/enforced" "${FAKE_STATE}/dns"
  [[ "$args" != *default-deny.json* ]] || touch "${FAKE_STATE}/enforced"
  [[ "$args" != *allow-dns.json* ]] || touch "${FAKE_STATE}/dns"
  exit
fi
if [[ "$args" == *'get daemonset cilium'* ]]; then
  version=1.20.2; [[ "${FAKE_CASE}" != wrong_version ]] || version=1.20.1
  printf '{"spec":{"template":{"spec":{"containers":[{"name":"cilium-agent","image":"quay.io/cilium/cilium:v%s@sha256:2939231d0d3e3ebddcd80fffa168b7ddcc78fdf0dc864d1c8c126ff523c54f01"}]}}}}\n' "$version"
  exit
fi
if [[ "$args" == *'get ciliumnetworkpolicies'* ]]; then
  status=True; [[ "${FAKE_CASE}" != invalid ]] || status=False
  [[ "${FAKE_CASE}" != unknown ]] || status=Unknown
  suffix=''; [[ "${FAKE_CASE}" != changed || ! -f "${FAKE_STATE}/first-read" ]] || suffix=changed
  touch "${FAKE_STATE}/first-read"
  floor=false; [[ ! -f "${FAKE_STATE}/enforced" ]] || floor=true
  dns=false; [[ ! -f "${FAKE_STATE}/dns" ]] || dns=true
  all="$(jq --argjson floor "$floor" --argjson dns "$dns" \
    '[.items[] | select((.metadata.name == "default-deny" and $floor) or (.metadata.name == "allow-dns" and $dns))]' \
    "${FAKE_STATE}/temp/cilium-deny-proof-123-1/runtime-policies.yaml")"
  allows="$(yq -o=json -I=0 '.' "${FAKE_STATE}/temp/cilium-deny-proof-123-1/allows.yaml")"
  count="$(jq --argjson all "$all" -n '$all | length + 1')"
  [[ "${FAKE_CASE}" != missing ]] || count=$((count - 1))
  jq -n --argjson all "$all" --argjson allows "$allows" --arg status "$status" --arg suffix "$suffix" --argjson count "$count" \
    '{items:[range($count) as $i | (($all + [$allows])[$i]) | .metadata.uid=("uid-"+.metadata.name+$suffix) | .metadata.generation=1 | .status={conditions:[{type:"Valid",status:$status}]}]}'
  exit
fi
if [[ "$args" == *'get pods client-allowed client-denied server-allowed server-denied'* ]]; then
  [[ "${FAKE_CASE}" != probe_list_error || ! -f "${FAKE_STATE}/enforced" ]] || exit 1
  pods="$(jq -n '{items:[
    {metadata:{name:"client-allowed",namespace:"deny-proof",uid:"pod-client-allowed"},status:{podIP:"10.1.0.8"}},
    {metadata:{name:"client-denied",namespace:"deny-proof",uid:"pod-client-denied"},status:{podIP:"10.1.0.9"}},
    {metadata:{name:"server-allowed",namespace:"deny-proof",uid:"pod-server-allowed"},status:{podIP:"10.1.0.10"}},
    {metadata:{name:"server-denied",namespace:"deny-proof",uid:"pod-server-denied"},status:{podIP:"10.1.0.11"}}
  ]} | .items |= map(
    .spec={containers:[{name:"probe",image:"docker.io/library/busybox:1.38.0-musl@sha256:ea2b9914a16a4ac1981994af97b318f7c7d4db76b580c56177f08bf76f4a0be8"}]} |
    .status += {phase:"Running",conditions:[{type:"Ready",status:"True"}],containerStatuses:[{name:"probe",ready:true,restartCount:0,containerID:("containerd://"+.metadata.uid),imageID:"docker-pullable://busybox@sha256:abc",state:{running:{startedAt:"2026-10-03T00:00:00Z"}}}]}
  )')"
  if [[ -f "${FAKE_STATE}/enforced" || "${FAKE_CASE}" == probe_baseline_not_ready ]]; then
    case "$FAKE_CASE" in
      target_unhealthy_after|probe_baseline_not_ready) pods="$(jq '.items |= map(if .metadata.name == "server-denied" then .status.conditions=[{type:"Ready",status:"False"}] | .status.containerStatuses[0].ready=false else . end)' <<<"$pods")" ;;
      target_uid_changed) pods="$(jq '.items |= map(if .metadata.name == "server-denied" then .metadata.uid="replacement-server" else . end)' <<<"$pods")" ;;
      target_ip_changed) pods="$(jq '.items |= map(if .metadata.name == "server-denied" then .status.podIP="10.1.0.99" else . end)' <<<"$pods")" ;;
      client_uid_changed) pods="$(jq '.items |= map(if .metadata.name == "client-denied" then .metadata.uid="replacement-client" else . end)' <<<"$pods")" ;;
      target_restarted) pods="$(jq '.items |= map(if .metadata.name == "server-denied" then .status.containerStatuses[0].restartCount=1 | .status.containerStatuses[0].containerID="containerd://replacement" else . end)' <<<"$pods")" ;;
    esac
  fi
  printf '%s\n' "$pods"
  exit
fi
if [[ "$args" == *'get pod server-allowed'* ]]; then echo 10.1.0.10; exit; fi
if [[ "$args" == *'get pod server-denied'* ]]; then echo 10.1.0.11; exit; fi
if [[ "$args" == *'exec '* ]]; then
  [[ "${FAKE_CASE}" != exec_error ]] || { echo 'API exec transport failed'; exit 1; }
  if [[ "$args" == *nslookup* ]]; then
    [[ "${FAKE_CASE}" != dns_error ]] || exit 1
    # DNS must not be used to interpret the floor-only phase: the floor is
    # supposed to deny it until the generated DNS companion is installed.
    [[ ! -f "${FAKE_STATE}/enforced" || -f "${FAKE_STATE}/dns" ]] || exit 1
    [[ "${FAKE_CASE}" != dns_after_floor_error || ! -f "${FAKE_STATE}/dns" ]] || exit 1
    echo 'Name: kubernetes.default.svc.cluster.local'; exit
  fi
  if [[ -f "${FAKE_STATE}/enforced" && "$args" == *'exec server-denied'* && "$args" == *'127.0.0.1'* && "${FAKE_CASE}" == target_loopback_error ]]; then
    printf 'wget: download timed out\nPROBE_EXIT=1\n'
    exit
  fi
  # Cilium enables directional default deny when ANY selecting policy has an
  # ingress/egress section unless that policy explicitly opts out. Model the
  # helper independently, so it cannot hide a floor with no datapath effect.
  blocked=false
  if [[ "$args" == *'exec client-denied'* || ( "$args" == *'exec client-allowed'* && "$args" == *10.1.0.11* ) ]]; then
    role=client-allowed; direction=egress
    if [[ "$args" == *'exec client-denied'* ]]; then role=server-allowed; direction=ingress; fi
    if [[ -f "${FAKE_STATE}/helper" ]] && yq -o=json -I=0 '.' "${FAKE_STATE}/temp/cilium-deny-proof-123-1/allows.yaml" |
      jq -e --arg role "$role" --arg direction "$direction" \
        'any(.specs[]; .endpointSelector.matchLabels.role == $role and (.[$direction] | length) > 0 and .enableDefaultDeny[$direction] != false)' >/dev/null; then blocked=true; fi
    if [[ -f "${FAKE_STATE}/enforced" && "${FAKE_CASE}" != floor_no_effect && ( "${FAKE_CASE}" != floor_egress_no_effect || "$direction" != egress ) ]] &&
      jq -e --arg role "$role" --arg direction "$direction" \
        'any(.items[]; .metadata.name == "default-deny" and (.spec.endpointSelector == {} or .spec.endpointSelector.matchLabels.role == $role) and (.spec[$direction] | length) > 0 and .spec.enableDefaultDeny[$direction] != false)' \
        "${FAKE_STATE}/temp/cilium-deny-proof-123-1/runtime-policies.yaml" >/dev/null; then blocked=true; fi
    # The generated DNS companion can itself impose egress default deny too.
    if [[ "$direction" == egress && -f "${FAKE_STATE}/dns" ]] &&
      jq -e 'any(.items[]; .metadata.name == "allow-dns" and .spec.endpointSelector == {} and (.spec.egress | length) > 0 and .spec.enableDefaultDeny.egress != false)' \
        "${FAKE_STATE}/temp/cilium-deny-proof-123-1/runtime-policies.yaml" >/dev/null; then blocked=true; fi
    [[ "${FAKE_CASE}" != allow_all ]] || blocked=false
  fi
  if [[ "$blocked" == true ]]; then
    if [[ "${FAKE_CASE}" == refused ]]; then printf "wget: can't connect: Connection refused\nPROBE_EXIT=1\n";
    elif [[ "${FAKE_CASE}" == other_timeout ]]; then printf 'wrapper: timed out\nPROBE_EXIT=124\n';
    else printf 'wget: download timed out\nPROBE_EXIT=1\n'; fi
  else
    [[ "${FAKE_CASE}" != baseline_error ]] || { printf 'wget: download timed out\nPROBE_EXIT=1\n'; exit; }
    printf 'deny-proof-ok\nPROBE_EXIT=0\n'
  fi
  exit
fi
if [[ "$args" == *'wait '* || "$args" == *'rollout status '* ]]; then exit; fi
exit 90
STUB
chmod +x "${work}/bin/"*
failures=0
check_case() {
  local name="$1" expected="$2" result=0 state="${work}/$1"
  mkdir -p "${state}/temp/cilium-deny-candidate"
  cp "${root}/k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml" "${state}/temp/cilium-deny-candidate/generator.yaml"
  cp "${root}/k8s/bases/infrastructure/controllers/oauth2-proxy/cilium-network-policy-default-deny.yaml" "${state}/temp/cilium-deny-candidate/flux-copy.yaml"
  # Plumbing lands before the candidate policy fix. Use an explicit valid
  # candidate fixture rather than silently inheriting main's legacy empty list.
  floor='{"endpointSelector":{},"ingress":[{}],"egress":[{}],"enableDefaultDeny":{"ingress":true,"egress":true}}'
  if [[ "$name" == floor_unselected ]]; then
    floor='{"endpointSelector":{"matchLabels":{"role":"no-probe"}},"ingress":[{}],"egress":[{}],"enableDefaultDeny":{"ingress":true,"egress":true}}'
  elif [[ "$name" == floor_not_enforcing ]]; then
    floor='{"endpointSelector":{},"ingress":[{}],"egress":[{}],"enableDefaultDeny":{"ingress":false,"egress":false}}'
  elif [[ "$name" == floor_egress_not_enforcing ]]; then
    floor='{"endpointSelector":{},"ingress":[{}],"egress":[{}],"enableDefaultDeny":{"ingress":true,"egress":false}}'
  fi
  FLOOR="$floor" yq -i '(.spec.rules[] | select(.name == "generate-default-deny") | .generate.data.spec) = (strenv(FLOOR) | from_json)' "${state}/temp/cilium-deny-candidate/generator.yaml"
  FLOOR="$floor" yq -i '.spec = (strenv(FLOOR) | from_json)' "${state}/temp/cilium-deny-candidate/flux-copy.yaml"
  if [[ "$name" == flux_extra_rules ]]; then
    yq -i '.specs = [{"endpointSelector":{},"ingress":[{}],"egress":[{}]}]' "${state}/temp/cilium-deny-candidate/flux-copy.yaml"
  fi
  jq -n --arg generator "$(sha256sum "${state}/temp/cilium-deny-candidate/generator.yaml" | cut -d' ' -f1)" --arg flux "$(sha256sum "${state}/temp/cilium-deny-candidate/flux-copy.yaml" | cut -d' ' -f1)" \
    '{schema:1,repository:"devantler-tech/platform",pr:4381,head:"01e40ecf795a3eaf6c0e30ba461a7253afb36462",author:"devantler",generator_sha256:$generator,flux_copy_sha256:$flux}' >"${state}/temp/cilium-deny-candidate/candidate.json"
  if [[ "$name" == tampered ]]; then printf '\n# changed after fetch\n' >>"${state}/temp/cilium-deny-candidate/generator.yaml"; fi
  if [[ "$name" == unconfirmed ]]; then confirm=no; else confirm=RUN_DISPOSABLE_CILIUM_PROOF; fi
  PATH="${work}/bin:${PATH}" FAKE_STATE="$state" FAKE_CASE="$name" RUNNER_TEMP="${state}/temp" \
    GITHUB_ACTIONS=true GITHUB_REPOSITORY=devantler-tech/platform GITHUB_REF=refs/heads/main \
    GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=1 GITHUB_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    CONFIRM_DISPOSABLE_CILIUM_PROOF="$confirm" \
    bash "$runner" run >"${state}/output" 2>&1 || result=$?
  if [[ "$result" != "$expected" ]]; then
    printf 'FAIL %s: want exit %s, got %s\n' "$name" "$expected" "$result"
    cat "${state}/output"
    failures=$((failures + 1))
  fi
  local receipt="${state}/temp/cilium-deny-proof-123-1/receipt/result.json"
  if [[ "$expected" == 0 ]]; then
    if ! jq -e '.verdict == "PASS" and .cleanup == "PASS" and .tests.dns and .tests.allowed_http and .tests.denied_ingress and .tests.denied_egress and .valid_cnp_count == 3 and .floor_valid_cnp_count == 2 and .floor_tests.pre_floor_http and .floor_tests.allowed_http and .floor_tests.denied_ingress and .floor_tests.denied_egress and .floor_tests.denied_target_http' "$receipt" >/dev/null; then
      printf 'FAIL %s: complete proof receipt absent\n' "$name"; failures=$((failures + 1))
    fi
    if ! jq -e '([.probe_bindings[].name] | sort) == ["client-allowed","client-denied","server-allowed","server-denied"] and all(.probe_bindings[]; (.uid | startswith("pod-")) and (.ip_sha256 | test("^[0-9a-f]{64}$")) and (.container_sha256 | test("^[0-9a-f]{64}$")) and (.image_sha256 | test("^[0-9a-f]{64}$")) and .restart_count == 0 and .ready == true) and .tests.denied_target_http' "$receipt" >/dev/null; then
      printf 'FAIL %s: healthy stable probe identity receipt absent\n' "$name"; failures=$((failures + 1))
    fi
    if rg -q '10[.]1[.]0[.]|containerd://' "$receipt"; then
      printf 'FAIL %s: receipt exposes private probe IP/container identity\n' "$name"; failures=$((failures + 1))
    fi
    # Read the recorded process boundary to prove all helper-only positive
    # controls and both floor-only denials were exercised before DNS was added.
    if ! awk '
      /apply -f .*allows[.]yaml$/ { phase="helper" }
      /apply -f .*default-deny[.]json$/ { if (phase!="helper") bad=1; phase="floor" }
      /apply -f .*allow-dns[.]json$/ { if (phase!="floor") bad=1; phase="dns" }
      /exec client-allowed .*http:\/\/10[.]1[.]0[.]10:8080/ { positive[phase]++ }
      /exec client-denied .*http:\/\/10[.]1[.]0[.]10:8080/ { ingress[phase]++ }
      /exec client-allowed .*http:\/\/10[.]1[.]0[.]11:8080/ { egress[phase]++ }
      /exec server-denied .*http:\/\/127[.]0[.]0[.]1:8080/ { target[phase]++ }
      /nslookup/ { dns[phase]++ }
      END {
        exit !(bad==0 && phase=="dns" &&
          positive["helper"]>=3 && ingress["helper"]>=3 && egress["helper"]>=3 && target["helper"]>=6 && dns["helper"]>=3 &&
          positive["floor"]>=3 && ingress["floor"]>=3 && egress["floor"]>=3 && target["floor"]>=6 && dns["floor"]==0 &&
          positive["dns"]>=3 && ingress["dns"]>=3 && egress["dns"]>=3 && target["dns"]>=6 && dns["dns"]>=3)
      }' "${state}/commands"; then
      printf 'FAIL %s: causal phase/control sequence absent\n' "$name"; failures=$((failures + 1))
    fi
  elif [[ -f "$receipt" ]] && jq -e '.verdict == "PASS" and .cleanup == "PASS"' "$receipt" >/dev/null; then
    printf 'FAIL %s: failure produced a green receipt\n' "$name"; failures=$((failures + 1))
  fi
  if [[ "$name" == floor_* ]]; then
    if ! rg -q '^FAIL: floor-only healthy target/admitted HTTP and independent ingress/egress denial did not converge$' "${state}/output" ||
       ! jq -e '.verdict == "FAIL" and .cleanup == "PASS" and .floor_valid_cnp_count == 0' "$receipt" >/dev/null ||
       rg -q 'apply -f .*allow-dns[.]json$' "${state}/commands"; then
      printf 'FAIL %s: witness did not reach and reject the isolated floor before DNS\n' "$name"; failures=$((failures + 1))
    fi
  elif [[ "$name" == dns_after_floor_error ]]; then
    if ! rg -q '^FAIL: complete healthy target/DNS/admitted/independent ingress and egress denial did not converge$' "${state}/output" ||
       ! jq -e '.verdict == "FAIL" and .cleanup == "PASS" and .floor_valid_cnp_count == 2 and .floor_tests.denied_ingress and .floor_tests.denied_egress' "$receipt" >/dev/null; then
      printf 'FAIL %s: final DNS failure bypassed the proven floor phase\n' "$name"; failures=$((failures + 1))
    fi
  fi
  if [[ "$name" == unconfirmed || "$name" == occupied || "$name" == tampered || "$name" == docker_error || "$name" == wrong_ksail ]]; then
    if [[ -f "${state}/commands" ]] && rg -q 'ksail .*cluster (create|delete)|kubectl' "${state}/commands"; then
      printf 'FAIL %s: rejected preflight touched a cluster\n' "$name"; failures=$((failures + 1))
    fi
  elif [[ "$name" != cleanup_error && -f "${state}/created" ]]; then
    printf 'FAIL %s: owned cluster survived cleanup\n' "$name"; failures=$((failures + 1))
  fi
  # Every Kubernetes call must use only the private context/config. Deleting
  # by ambient context, a changed provider or a whole Docker prune is unsafe.
  if [[ -f "${state}/commands" ]] && rg '^kubectl ' "${state}/commands" | rg -v -- '--kubeconfig .*/cilium-deny-proof-123-1/kubeconfig --context kind-deny-proof-123-1' >/dev/null; then
    printf 'FAIL %s: ambient Kubernetes call\n' "$name"; failures=$((failures + 1))
  fi
}
check_case success 0
for name in unconfirmed occupied docker_error wrong_ksail tampered wrong_version invalid unknown missing exec_error dns_error baseline_error allow_all refused other_timeout cleanup_error changed target_unhealthy_after target_loopback_error target_uid_changed target_ip_changed client_uid_changed target_restarted probe_list_error probe_baseline_not_ready flux_extra_rules floor_unselected floor_not_enforcing floor_no_effect floor_egress_not_enforcing floor_egress_no_effect dns_after_floor_error; do check_case "$name" 1; done
[[ "$failures" == 0 ]] || { printf '%s failures\n' "$failures"; exit 1; }
echo 'PASS: isolated proof refuses incomplete/false traffic evidence and cleans only its owned cluster'
