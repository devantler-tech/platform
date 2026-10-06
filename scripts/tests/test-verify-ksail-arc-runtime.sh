#!/usr/bin/env bash
set -euo pipefail
umask 077
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin" "$scratch/scripts" "$scratch/k8s/providers/hetzner/infrastructure/controllers" \
  "$scratch/k8s/bases/infrastructure/actions-runners" \
  "$scratch/k8s/bases/infrastructure/controllers/actions-runner-controller"
cp "$root/scripts/verify-ksail-arc-runtime.sh" "$scratch/scripts/"
if [[ -e "$root/scripts/wait-for-ksail-arc-registration.sh" ]]; then
  cp "$root/scripts/wait-for-ksail-arc-registration.sh" "$scratch/scripts/"
fi
cp "$root/k8s/bases/infrastructure/actions-runners/helm-release.yaml" \
  "$scratch/k8s/bases/infrastructure/actions-runners/"
cp "$root/scripts/ksail-arc-job-metrics.sh" "$scratch/scripts/"
cp "$root/k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml" \
  "$scratch/k8s/bases/infrastructure/controllers/actions-runner-controller/"
cp "$root/k8s/providers/hetzner/infrastructure/retained-ksail-analysis/helm-release.yaml" \
  "$scratch/retained-release.yaml"
yq -i '.spec.suspend=false' "$scratch/k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml"
go build -o "$scratch/verifier" "$root/scripts/verify-ksail-arc-runtime"
export ARC_TEST_ROOT="$scratch"
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}' >"$scratch/runtime-image"
runtime_digest="sha256:$(shasum -a 256 "$scratch/runtime-image" | cut -d' ' -f1)"
export runtime_digest
jq -cn --arg child "$runtime_digest" '{schemaVersion:2,mediaType:"application/vnd.oci.image.index.v1+json",
  manifests:[{digest:$child,platform:{os:"linux",architecture:"amd64"}}]}' >"$scratch/image"
digest="sha256:$(shasum -a 256 "$scratch/image" | cut -d' ' -f1)"
export digest
image="ghcr.io/devantler-tech/ksail-analysis-runner@$digest"
image="$image" yq -i '.spec.values.template.spec.containers[0].image=strenv(image) |
  .spec.values.template.spec.initContainers[0].image=strenv(image) | .spec.suspend=false' \
  "$scratch/k8s/bases/infrastructure/actions-runners/helm-release.yaml"
yq -o=json '.spec.values' "$scratch/k8s/bases/infrastructure/actions-runners/helm-release.yaml" \
  | jq '{metadata:{generation:1,annotations:{"runner-scale-set-id":"1",
      "actions.github.com/runner-group-name":"platform",
      "actions.github.com/runner-scale-set-name":"platform-linux"}},
    status:{phase:"Running",observedGeneration:1},spec:.} |
    .spec.template.spec.serviceAccountName="platform-linux-gha-rs-no-permission" |
    .spec.template.spec.volumes[2].configMap.name="ksail-arc-job-metrics-abc123xyz4"' >"$scratch/ars"
cat >"$scratch/scripts/wait-for-platform-flux-revision.sh" <<'SH'
#!/usr/bin/env bash
[[ "$1" == "$digest" ]]
SH
cat >"$scratch/bin/timeout" <<'SH'
#!/usr/bin/env bash
export ARC_TEST_TIMEOUT=$1
if [[ "$1" == 660s ]]; then touch "$ARC_TEST_ROOT/registration-budget"; fi
shift
exec "$@"
SH
cat >"$scratch/bin/sleep" <<'SH'
#!/usr/bin/env bash
[[ "$1" == 10 ]] || exit 98
case "$ARC_TEST_CASE" in no-registration|registration-timeout|stale-flux) exit 124 ;; esac
SH
cat >"$scratch/bin/date" <<'SH'
#!/usr/bin/env bash
printf '2026-10-05T22:00:00.000000000Z\n'
SH
cat >"$scratch/bin/go" <<'SH'
#!/usr/bin/env bash
[[ "$1" == run && "$2" == ./scripts/verify-ksail-arc-runtime ]] || exit 91
shift 2
exec "$ARC_TEST_ROOT/verifier" "$@"
SH
cat >"$scratch/bin/cosign" <<'SH'
#!/usr/bin/env bash
[[ "$*" == *"--certificate-identity https://github.com/devantler-tech/ksail/.github/workflows/publish-ksail-analysis-runner.yaml@refs/heads/main --certificate-oidc-issuer https://token.actions.githubusercontent.com ghcr.io/devantler-tech/ksail-analysis-runner@$digest" ]] || exit 92
[[ "$ARC_TEST_CASE" != bad-signature ]] || exit 1
SH
cat >"$scratch/bin/docker" <<'SH'
#!/usr/bin/env bash
if [[ "$*" == "buildx imagetools inspect ghcr.io/devantler-tech/ksail-analysis-runner@$digest --raw" ]]; then
  cat "$ARC_TEST_ROOT/image"
elif [[ "$*" == "buildx imagetools inspect ghcr.io/devantler-tech/ksail-analysis-runner@$runtime_digest --raw" ]]; then
  cat "$ARC_TEST_ROOT/runtime-image"
else exit 93; fi
[[ "$ARC_TEST_CASE" != tampered-image ]] || printf ' '
SH
cat >"$scratch/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == kustomize ]]; then
  [[ "${ARC_TEST_CASE:-}" != source-render-failure ]] || exit 1
  case "$2" in
    k8s/providers/hetzner/infrastructure/controllers)
      aggregate="$ARC_TEST_ROOT/$2/kustomization.yaml"
      release="$ARC_TEST_ROOT/k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml"
      reference='actions-runner-controller' ;;
    k8s/providers/hetzner/infrastructure)
      aggregate="$ARC_TEST_ROOT/$2/kustomization.yaml"
      release="$ARC_TEST_ROOT/k8s/bases/infrastructure/actions-runners/helm-release.yaml"
      reference='actions-runners' ;;
    *) exit 0 ;;
  esac
  references=$(yq -o=json '(.resources // []) + (.bases // [])' "$aggregate")
  if [[ "$2" == k8s/providers/hetzner/infrastructure ]]; then
    case "${ARC_TEST_CASE:-}" in
      rendered-retained-drain|rendered-drained-controller|rendered-drained-controller-wrong-scope|retained-active-min|retained-active-max|retained-suspended|retained-missing-bound|retained-wrong-scale-set)
        printf '\n---\n'
        case "$ARC_TEST_CASE" in
          retained-active-min) yq '.spec.values.minRunners=1' "$ARC_TEST_ROOT/retained-release.yaml" ;;
          retained-active-max) yq '.spec.values.maxRunners=1' "$ARC_TEST_ROOT/retained-release.yaml" ;;
          retained-suspended) yq '.spec.suspend=true' "$ARC_TEST_ROOT/retained-release.yaml" ;;
          retained-missing-bound) yq 'del(.spec.values.maxRunners)' "$ARC_TEST_ROOT/retained-release.yaml" ;;
          retained-wrong-scale-set) yq '.spec.values.runnerScaleSetName="unapproved"' "$ARC_TEST_ROOT/retained-release.yaml" ;;
          *) cat "$ARC_TEST_ROOT/retained-release.yaml" ;;
        esac
        printf '\n---\n'
        ;;
    esac
  fi
  if jq -e --arg component "$reference" 'any(.[]; contains($component))' <<<"$references" >/dev/null; then
    case "${ARC_TEST_CASE:-}" in
      rendered-suspended) yq '.spec.suspend=true' "$release" ;;
      rendered-drained-controller) yq '.metadata.annotations."platform.devantler.tech/arc-recovery"="drain-only"' "$release" ;;
      rendered-drained-controller-wrong-scope) yq '.metadata.annotations."platform.devantler.tech/arc-recovery"="drain-only" | .spec.values.flags.watchSingleNamespace="unapproved"' "$release" ;;
      rendered-wrong-image) yq '.spec.values.template.spec.containers[0].image="unverified:latest"' "$release" ;;
      *) cat "$release" ;;
    esac
  fi
  exit 0
fi
[[ "$1" == --context && "$2" == admin@prod ]] || exit 94
touch "$ARC_TEST_ROOT/runtime-access"
shift 3 # context pair and request-timeout
args="$*"
case "$args" in
  *'get providerconfigs.github.m.upbound.io --all-namespaces '*)
    jq -cn '{items:[{apiVersion:"github.m.upbound.io/v1beta1",kind:"ProviderConfig",
      metadata:{name:"arc-runtime-platform-app",namespace:"arc-runners"},
      spec:{credentials:{source:"Secret",secretRef:{namespace:"arc-runners",name:"arc-github-app",key:"provider-credentials"}}}}]}' ;;
  *'get clusterproviderconfigs.github.m.upbound.io '*|*'get providerconfigs.github.upbound.io '*)
    printf '{"items":[]}' ;;
  *'get deployment cluster-autoscaler-hetzner-cluster-autoscaler '*)
    printf '%s\n' --cloud-provider=hetzner --max-nodes-total=9 --nodes=0:1:cx53:fsn1:autoscale-arc-runners
    [[ "$ARC_TEST_CASE" != wider-ceiling ]] || printf '%s\n' --max-nodes-total=10 ;;
  *'get kustomization '*)
    count=0
    [[ ! -e "$ARC_TEST_ROOT/flux-reads" ]] || count=$(cat "$ARC_TEST_ROOT/flux-reads")
    count=$((count + 1))
    printf '%s\n' "$count" >"$ARC_TEST_ROOT/flux-reads"
    if [[ "$ARC_TEST_CASE" == stale-flux || ( "$ARC_TEST_CASE" == delayed-flux && "$count" -le 2 ) ]]; then
      printf '{"metadata":{"generation":2},"status":{"observedGeneration":1,"lastAppliedRevision":"latest@old","conditions":[{"type":"Ready","status":"True"}]}}'
      exit 0
    fi
    jq -n --arg revision "latest@$digest" '{metadata:{generation:1},status:{observedGeneration:1,
      lastAppliedRevision:$revision,conditions:[{type:"Ready",status:"True"}]}}' ;;
  *'get runnergroups.actions.github.m.upbound.io platform '*)
    jq -cn '{apiVersion:"actions.github.m.upbound.io/v1alpha1",kind:"RunnerGroup",
      metadata:{name:"platform",namespace:"arc-runners",uid:"group-uid",generation:2,annotations:{"crossplane.io/external-name":"27"}},
      spec:{providerConfigRef:{name:"arc-runtime-platform-app",kind:"ProviderConfig"},forProvider:{name:"platform",visibility:"selected",allowsPublicRepositories:true,restrictedToWorkflows:true,selectedRepositoryIds:[737584922],selectedWorkflows:["devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"]}},
      status:{atProvider:{id:"27",name:"platform",default:false,inherited:false,visibility:"selected",allowsPublicRepositories:true,restrictedToWorkflows:true,selectedRepositoryIds:[737584922],selectedWorkflows:["devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"],
        runnersUrl:"https://api.github.com/orgs/devantler-tech/actions/runner-groups/27/runners",
        selectedRepositoriesUrl:"https://api.github.com/orgs/devantler-tech/actions/runner-groups/27/repositories"},
        conditions:[{type:"Ready",status:"True",observedGeneration:2},{type:"Synced",status:"True",observedGeneration:2}]}}' ;;
  *'get autoscalingrunnerset.actions.github.com '*)
    count=0
    [[ ! -e "$ARC_TEST_ROOT/registration-reads" ]] || count=$(cat "$ARC_TEST_ROOT/registration-reads")
    count=$((count + 1))
    printf '%s\n' "$count" >"$ARC_TEST_ROOT/registration-reads"
    if [[ "$ARC_TEST_CASE" == registration-timeout || ( "$ARC_TEST_CASE" == delayed-registration && "$count" -eq 1 ) ]]; then
      jq '.status.phase="Pending" | .status.observedGeneration=0 | .metadata.annotations["runner-scale-set-id"]="0"' "$ARC_TEST_ROOT/ars"
      exit 0
    fi
    case "$ARC_TEST_CASE" in
      no-registration) jq '.metadata.annotations["runner-scale-set-id"]="0"' "$ARC_TEST_ROOT/ars" ;;
      unexpected-hook) jq '.spec.template.spec.containers[0].env[0].value="/unverified"' "$ARC_TEST_ROOT/ars" ;;
      writable-metrics) jq '.spec.template.spec.containers[0].volumeMounts[2].readOnly=false' "$ARC_TEST_ROOT/ars" ;;
      wrong-metrics-configmap) jq '.spec.template.spec.volumes[2].configMap.name="unverified"' "$ARC_TEST_ROOT/ars" ;;
      *) cat "$ARC_TEST_ROOT/ars" ;;
    esac ;;
  *'get configmap ksail-arc-job-metrics-'*)
    jq -cn --rawfile script "$ARC_TEST_ROOT/scripts/ksail-arc-job-metrics.sh" \
      --arg scenario "$ARC_TEST_CASE" '{metadata:{uid:"metrics-uid",annotations:{"kustomize.toolkit.fluxcd.io/substitute":"disabled"}},immutable:($scenario!="mutable-metrics"),
      data:{"job-metrics.sh":(if $scenario=="tampered-metrics" then "unverified" else $script end)}}' ;;
  *'get resourcequotas '*)
    if [[ "$ARC_TEST_CASE" == full-quota ]]; then
      printf '{"items":[{"spec":{"hard":{"pods":"1"}},"status":{"hard":{"pods":"1"},"used":{"pods":"1"}}}]}'
    elif [[ "$ARC_TEST_CASE" == stale-quota ]]; then
      printf '{"items":[{"spec":{"hard":{"pods":"1"}},"status":{"hard":{"pods":"3"},"used":{"pods":"0"}}}]}'
    else printf '{"items":[]}'; fi ;;
  *'get pods -l platform.devantler.tech/arc-role=runner '*|*'get pods -l platform.devantler.tech/arc-runtime-probe'*)
    printf '{"items":[]}' ;;
  'get nodes -o json')
    if [[ "$ARC_TEST_CASE" == failed-node-cleanup && -e "$ARC_TEST_ROOT/deleted" ]]; then exit 1; fi
    if [[ "$ARC_TEST_CASE" == unknown-create && -e "$ARC_TEST_ROOT/live-pod" ]]; then exit 1; fi
    if [[ "$ARC_TEST_CASE" == replacement-race && -e "$ARC_TEST_ROOT/replacement-preserved" ]]; then exit 1; fi
    if [[ -e "$ARC_TEST_ROOT/live-pod" ]]; then
      printf '{"items":[{"metadata":{"name":"autoscale-arc-runners-0123456789abcdef","uid":"autoscale-arc-runners-0123456789abcdef-uid","labels":{"platform.devantler.tech/ci-runner":"enabled"}}}]}'
    else
      printf '{"items":[{"metadata":{"name":"baseline-node","uid":"baseline-uid","labels":{}}}]}'
    fi ;;
  'create --dry-run=server -f '*)
    file=$4
    if jq -e '.spec.containers[0].securityContext.privileged == true or any(.spec.volumes[]; .hostPath != null)' "$file" >/dev/null; then
      if [[ "$ARC_TEST_CASE" == admission-transport ]]; then printf 'connection refused\n' >&2
      else printf 'violates PodSecurity restricted: privileged hostPath\n' >&2; fi
      exit 1
    fi ;;
  'create -f '*)
    cp "$3" "$ARC_TEST_ROOT/desired-pod"
    jq --arg image "ghcr.io/devantler-tech/ksail-analysis-runner@$digest" '
      .metadata.uid="owned-uid" | .spec.nodeName="autoscale-arc-runners-0123456789abcdef" |
      .status={podIP:"192.0.2.10",
        containerStatuses:[{name:"runner",ready:true,restartCount:0,containerID:"runtime-id",imageID:$image}],
        initContainerStatuses:[{name:"init-runner-home",restartCount:0,imageID:$image,state:{terminated:{exitCode:0}}}]}' \
      "$3" >"$ARC_TEST_ROOT/live-pod"
    if [[ "$ARC_TEST_CASE" == unknown-create ]]; then exit 1; fi
    cat "$ARC_TEST_ROOT/live-pod" ;;
  *'wait pod/'*'--for=condition=Ready --timeout=600s')
    [[ "$ARC_TEST_TIMEOUT" == 630s ]] || exit 95 ;;
  *'wait pod/'*'--for=delete --timeout=25s') ;;
  *'get pod arc-proof-'*)
    [[ -e "$ARC_TEST_ROOT/live-pod" ]] || exit 0
    if [[ "$ARC_TEST_CASE" == mutated-init ]]; then
      jq '.spec.initContainers[0].securityContext.privileged=true' "$ARC_TEST_ROOT/live-pod"
    else cat "$ARC_TEST_ROOT/live-pod"; fi ;;
  'delete --raw='*)
    [[ "$ARC_TEST_CASE" != replacement-race ]] || { touch "$ARC_TEST_ROOT/replacement-preserved"; exit 1; }
    jq -e '.kind=="DeleteOptions" and .preconditions.uid=="owned-uid" and .gracePeriodSeconds==5' "$4" >/dev/null || exit 96
    rm "$ARC_TEST_ROOT/live-pod"
    touch "$ARC_TEST_ROOT/deleted" ;;
  'get node autoscale-arc-runners-0123456789abcdef '*)
    jq -n '{metadata:{name:"autoscale-arc-runners-0123456789abcdef",uid:"autoscale-arc-runners-0123456789abcdef-uid",labels:{"platform.devantler.tech/ci-runner":"enabled","node.kubernetes.io/instance-type":"cx53"}},
      spec:{taints:[{key:"platform.devantler.tech/ci-runner",value:"enabled",effect:"NoSchedule"}]},
      status:{nodeInfo:{operatingSystem:"linux",architecture:"amd64"},allocatable:{cpu:"15",memory:"30Gi","ephemeral-storage":"100Gi"},
        conditions:[{type:"Ready",status:"True"}]}}' ;;
  'get pods -A --field-selector spec.nodeName=autoscale-arc-runners-0123456789abcdef '*)
    printf 'P\towned-uid\tRunning\nR\t3\t12Gi\t32Gi\nP\tsystem-uid\tRunning\nR\t100m\t1Gi\t1Gi\n'
    if [[ "$ARC_TEST_CASE" == insufficient-memory ]]; then printf 'R\t100m\t25Gi\t1Gi\n'; fi ;;
  *'get pods -l k8s-app=cilium '*)
    printf '{"items":[{"metadata":{"name":"observer","uid":"observer-uid"},"spec":{"nodeName":"autoscale-arc-runners-0123456789abcdef"},
      "status":{"podIP":"192.0.2.11","containerStatuses":[{"name":"cilium-agent","ready":true,"restartCount":0,"containerID":"observer-id"}],"conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
  *'get configmap cilium-config '*)
    printf '{"data":{"cluster-name":"fixture"}}' ;;
  *'exec observer -c cilium-agent -- hubble status '*) ;;
  *'get pods -l k8s-app=kube-dns '*)
    printf '{"items":[{"metadata":{"name":"dns","uid":"dns-uid"},"spec":{"nodeName":"baseline-node"},
      "status":{"podIP":"192.0.2.12","containerStatuses":[{"name":"dns","ready":true,"restartCount":0,"containerID":"dns-id"}],"conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
  *'get endpoints kubernetes '*)
    printf '{"subsets":[{"addresses":[{"ip":"192.0.2.13"}],"ports":[{"name":"https","port":6443}]}]}' ;;
  'get --raw /api/v1/namespaces/kube-system/pods/dns:8181/proxy/ready')
    [[ "$ARC_TEST_CASE" != unhealthy-target ]] || { printf 'not ready'; exit 0; }
    printf OK ;;
  '--server=https://192.0.2.13:6443 get --raw /readyz') printf ok ;;
  *'exec arc-proof-'*'-- curl '*)
    if [[ "$ARC_TEST_CASE" == curl-transport ]]; then exit 1; fi
    if [[ "$ARC_TEST_CASE" == allowed-egress ]]; then exit 0; fi
    exit 28 ;;
  *'exec arc-proof-'*'-- test -f /tmp/arc-proof-ready') ;;
  *'exec arc-proof-'*'-- /etc/ksail-arc-metrics/job-metrics.sh')
    mkdir -p "$ARC_TEST_ROOT/cgroup"
    printf '15032385536\n' >"$ARC_TEST_ROOT/cgroup/memory.max"
    if [[ "$ARC_TEST_CASE" == failed-metrics-hook ]]; then
      printf '0\n' >"$ARC_TEST_ROOT/cgroup/memory.peak"
    else printf '9876543210\n' >"$ARC_TEST_ROOT/cgroup/memory.peak"; fi
    printf 'low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n' >"$ARC_TEST_ROOT/cgroup/memory.events"
    source "$ARC_TEST_ROOT/scripts/ksail-arc-job-metrics.sh"
    arc_job_metrics "$ARC_TEST_ROOT/cgroup"
    touch "$ARC_TEST_ROOT/metrics-executed" ;;
  *'exec observer -c cilium-agent -- hubble observe '*)
    destination=192.0.2.12 port=8181 target_ns=kube-system target_pod=dns
    [[ "$args" != *'--to-ip 192.0.2.13'* ]] || { destination=192.0.2.13; port=6443; target_ns=''; target_pod=''; }
    verdict=DROPPED source=192.0.2.10
    [[ "$ARC_TEST_CASE" != forwarded-flow ]] || verdict=FORWARDED
    [[ "$ARC_TEST_CASE" != wrong-source ]] || source=192.0.2.99
    jq -n --arg verdict "$verdict" --arg source "$source" --arg destination "$destination" --argjson port "$port" \
      --arg target_ns "$target_ns" --arg target_pod "$target_pod" '{flow:{verdict:$verdict,drop_reason_desc:"POLICY_DENIED",
      traffic_direction:"EGRESS",source:{namespace:"arc-runners",pod_name:"arc-proof-123-1"},
      destination:{namespace:$target_ns,pod_name:$target_pod},IP:{source:$source,destination:$destination},
      l4:{TCP:{destination_port:$port,flags:{SYN:true}}},node_name:"fixture/autoscale-arc-runners-0123456789abcdef",time:"2026-10-05T22:00:00Z"}}'
    [[ "$ARC_TEST_CASE" != observer-loss ]] || printf '{"lost_events":{"num_events_lost":"1"}}'
    [[ "$ARC_TEST_CASE" != observer-diagnostics ]] || printf 'observer warning\n' >&2 ;;
  *'get pod observer '*)
    printf '{"metadata":{"name":"observer","uid":"observer-uid"},"spec":{"nodeName":"autoscale-arc-runners-0123456789abcdef"},
      "status":{"podIP":"192.0.2.11","containerStatuses":[{"name":"cilium-agent","ready":true,"restartCount":0,"containerID":"observer-id"}]}}' ;;
  *'get pod dns '*)
    printf '{"metadata":{"name":"dns","uid":"dns-uid"},"spec":{"nodeName":"baseline-node"},
      "status":{"podIP":"192.0.2.12","containerStatuses":[{"name":"dns","ready":true,"restartCount":0,"containerID":"dns-id"}]}}' ;;
  *) printf 'unrecognized fixture command\n' >&2; exit 97 ;;
esac
SH
chmod +x "$scratch/bin/"*

run_case() {
  local name=$1 expected=$2 code=0
  rm -f "$scratch/live-pod" "$scratch/deleted" "$scratch/replacement-preserved" "$scratch/runtime-access"
  rm -f "$scratch/flux-reads" "$scratch/registration-reads" "$scratch/registration-budget"
  rm -f "$scratch/metrics-executed"
  (
    cd "$scratch"
    PATH="$scratch/bin:$PATH" GITHUB_ACTIONS=true GITHUB_REPOSITORY=devantler-tech/platform \
      GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=1 PLATFORM_MANIFEST_DIGEST="$digest" ARC_TEST_CASE="$name" \
      bash scripts/verify-ksail-arc-runtime.sh --if-active
  ) >"$scratch/stdout" 2>"$scratch/stderr" || code=$?
  if [[ "$expected" == pass ]]; then
    [[ "$code" == 0 && -e "$scratch/deleted" && -e "$scratch/metrics-executed" ]] || { printf 'FAIL: %s (exit %s)\n' "$name" "$code" >&2; cat "$scratch/stderr" >&2; exit 1; }
    grep -q '^PASS: ARC registration' "$scratch/stdout" || exit 1
  else
    [[ "$code" != 0 ]] || { printf 'FAIL: accepted %s\n' "$name" >&2; exit 1; }
    case "$name" in
      replacement-race) [[ -e "$scratch/replacement-preserved" && ! -e "$scratch/deleted" && "$code" == 4 ]] || exit 1 ;;
      unknown-create) [[ -e "$scratch/live-pod" && ! -e "$scratch/deleted" && "$code" == 4 ]] || exit 1 ;;
      failed-node-cleanup) [[ -e "$scratch/deleted" && "$code" == 4 ]] || exit 1 ;;
    esac
  fi
  printf 'PASS: %s\n' "$name"
}

printf 'resources: []\n' >"$scratch/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
printf 'resources: []\n' >"$scratch/k8s/providers/hetzner/infrastructure/kustomization.yaml"
(
  cd "$scratch"
  PATH="$scratch/bin:$PATH" bash scripts/verify-ksail-arc-runtime.sh --if-active
) >"$scratch/inactive"
grep -Fq 'inactive source; no runtime access' "$scratch/inactive"
[[ ! -e "$scratch/runtime-access" ]]
printf 'resources: ["../../../../bases/infrastructure/controllers/actions-runner-controller/"]\n' \
  >"$scratch/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
(
  cd "$scratch"
  PATH="$scratch/bin:$PATH" ARC_TEST_CASE=rendered-suspended bash scripts/verify-ksail-arc-runtime.sh --if-active
) >"$scratch/retained-controller"
grep -Fq 'inactive source; no runtime access' "$scratch/retained-controller"
[[ ! -e "$scratch/runtime-access" ]]
(
  cd "$scratch"
  PATH="$scratch/bin:$PATH" ARC_TEST_CASE=rendered-drained-controller bash scripts/verify-ksail-arc-runtime.sh --if-active
) >"$scratch/drained-controller"
grep -Fq 'inactive source; no runtime access' "$scratch/drained-controller"
[[ ! -e "$scratch/runtime-access" ]]
if (cd "$scratch"; PATH="$scratch/bin:$PATH" ARC_TEST_CASE=rendered-drained-controller-wrong-scope bash scripts/verify-ksail-arc-runtime.sh --if-active) \
  >"$scratch/wrong-scope-out" 2>"$scratch/wrong-scope-error"; then exit 1; fi
grep -Fq 'FAIL at partial-activation' "$scratch/wrong-scope-error"
[[ ! -e "$scratch/runtime-access" ]]
if (cd "$scratch"; PATH="$scratch/bin:$PATH" bash scripts/verify-ksail-arc-runtime.sh --if-active) \
  >"$scratch/active-controller-out" 2>"$scratch/active-controller-error"; then exit 1; fi
grep -Fq 'FAIL at partial-activation' "$scratch/active-controller-error"
[[ ! -e "$scratch/runtime-access" ]]
printf 'resources: ["../../../bases/infrastructure/actions-runners/"]\n' \
  >"$scratch/k8s/providers/hetzner/infrastructure/kustomization.yaml"
if (cd "$scratch"; PATH="$scratch/bin:$PATH" GITHUB_ACTIONS=false bash scripts/verify-ksail-arc-runtime.sh --if-active) \
  >"$scratch/unauthorized-out" 2>"$scratch/unauthorized-error"; then exit 1; fi
grep -Fq 'FAIL at deployment-identity' "$scratch/unauthorized-error"
run_case rendered-retained-drain pass
for name in retained-active-min retained-active-max retained-suspended retained-missing-bound retained-wrong-scale-set; do
  run_case "$name" fail
  grep -Fq 'FAIL at retained-source-state' "$scratch/stderr"
  [[ ! -e "$scratch/runtime-access" && ! -e "$scratch/live-pod" ]]
done
run_case complete-proof pass
run_case delayed-flux pass
[[ $(cat "$scratch/flux-reads") -gt 2 && -e "$scratch/registration-budget" ]]
run_case delayed-registration pass
[[ $(cat "$scratch/registration-reads") -gt 2 && -e "$scratch/registration-budget" ]]
for name in registration-timeout stale-flux; do
  run_case "$name" fail
  [[ -e "$scratch/registration-budget" && ! -e "$scratch/live-pod" && ! -e "$scratch/deleted" ]]
done
cp "$scratch/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml" "$scratch/controller-aggregate"
cp "$scratch/k8s/providers/hetzner/infrastructure/kustomization.yaml" "$scratch/runner-aggregate"
sed 's/^resources:/bases:/' "$scratch/controller-aggregate" >"$scratch/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
sed 's/^resources:/bases:/' "$scratch/runner-aggregate" >"$scratch/k8s/providers/hetzner/infrastructure/kustomization.yaml"
run_case rendered-hidden-activation fail
[[ ! -e "$scratch/runtime-access" ]]
cp "$scratch/controller-aggregate" "$scratch/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
cp "$scratch/runner-aggregate" "$scratch/k8s/providers/hetzner/infrastructure/kustomization.yaml"
for name in source-render-failure rendered-suspended rendered-wrong-image; do
  run_case "$name" fail
  [[ ! -e "$scratch/runtime-access" ]]
done
# Hetzner formats rand.Int63 with %x, so valid suffixes contain one to sixteen
# hex digits. Exercise the complete lifecycle with the shortest legal name.
cp "$scratch/bin/kubectl" "$scratch/kubectl-original"
for suffix in a 0123456789abcdef0 nothex; do
  sed "s/autoscale-arc-runners-0123456789abcdef/autoscale-arc-runners-$suffix/g" \
    "$scratch/kubectl-original" >"$scratch/bin/kubectl"
  if [[ "$suffix" == a ]]; then run_case short-node-suffix pass
  else run_case "invalid-node-suffix-$suffix" fail; fi
done
sed 's/autoscale-arc-runners-0123456789abcdef/autoscale-other-pool-0123456789abcdef/g' \
  "$scratch/kubectl-original" >"$scratch/bin/kubectl"
run_case wrong-node-pool fail
cp "$scratch/kubectl-original" "$scratch/bin/kubectl"
for name in no-registration unexpected-hook writable-metrics wrong-metrics-configmap mutable-metrics tampered-metrics \
  failed-metrics-hook bad-signature tampered-image wider-ceiling full-quota stale-quota admission-transport mutated-init \
  unknown-create replacement-race insufficient-memory unhealthy-target curl-transport allowed-egress \
  forwarded-flow wrong-source observer-loss observer-diagnostics failed-node-cleanup; do
  run_case "$name" fail
done
printf 'PASS: inactive and unauthorized sources never reach runtime access\n'
